package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// WriteTimeout bounds every database write a check goroutine makes. Writes
// run on a context detached from the signal context, so a result that is
// ready when the shutdown signal arrives is still stored (or its lease
// released) instead of being aborted with it.
const WriteTimeout = 5 * time.Second

// Store is the lease lifecycle the scheduler needs from the database. It is
// implemented by repository.SchedulerStore.
type Store interface {
	// Claim leases up to batchSize due monitors to workerID. Each returned
	// monitor's NextCheckAt is the claimed value Record needs.
	Claim(ctx context.Context, workerID string, batchSize int32, lease time.Duration) ([]domain.Monitor, error)
	// Record stores a check result and releases the lease. It returns
	// ErrLeaseLost if workerID no longer holds the lease.
	Record(ctx context.Context, workerID string, monitorID uuid.UUID,
		claimedNextCheckAt time.Time, outcome domain.CheckOutcome) error
	// Release gives a lease back without a result; the monitor stays due.
	Release(ctx context.Context, workerID string, monitorID uuid.UUID) error
}

// Checker runs one check. It is implemented by checker.Checker.
type Checker interface {
	// Check returns a non-nil error only when ctx was cancelled; that is not
	// a check result and is never recorded.
	Check(ctx context.Context, m domain.Monitor) (domain.CheckOutcome, error)
}

// Config holds the scheduler's settings. Every field is required; the values
// come from the validated config.Worker and config.Config.ShutdownTimeout.
type Config struct {
	// WorkerID becomes monitors.lease_owner for every claim.
	WorkerID string
	// Concurrency is the most checks that run at once (WORKER_CONCURRENCY).
	Concurrency int
	// BatchSize is the most monitors one claim asks for (WORKER_BATCH_SIZE).
	BatchSize int
	// PollInterval is the time between claims (WORKER_POLL_INTERVAL).
	PollInterval time.Duration
	// LeaseDuration is the requested lease (WORKER_LEASE_DURATION).
	LeaseDuration time.Duration
	// ShutdownTimeout is how long in-flight checks may keep running after
	// the signal before they are cancelled (SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
}

func (c Config) validate() error {
	var errs []error
	if c.WorkerID == "" {
		errs = append(errs, errors.New("worker id is empty"))
	}
	if c.Concurrency < 1 {
		errs = append(errs, fmt.Errorf("concurrency %d < 1", c.Concurrency))
	}
	if c.BatchSize < 1 {
		errs = append(errs, fmt.Errorf("batch size %d < 1", c.BatchSize))
	}
	if c.PollInterval <= 0 {
		errs = append(errs, fmt.Errorf("poll interval %s <= 0", c.PollInterval))
	}
	if c.LeaseDuration <= 0 {
		errs = append(errs, fmt.Errorf("lease duration %s <= 0", c.LeaseDuration))
	}
	if c.ShutdownTimeout < 0 {
		errs = append(errs, fmt.Errorf("shutdown timeout %s < 0", c.ShutdownTimeout))
	}
	return errors.Join(errs...)
}

// Scheduler claims due monitors and checks them with bounded concurrency.
//
// Concurrency is bounded by a token channel with capacity Concurrency. The
// poll loop is its only acquirer, so cap - len is an exact count of free
// slots: a claim never asks for more monitors than can start immediately,
// nothing claimed waits in a queue, and a lease cannot run out before its
// check starts.
type Scheduler struct {
	store   Store
	checker Checker
	cfg     Config
	log     *slog.Logger
	tokens  chan struct{}
}

// New builds a Scheduler. It panics if cfg is invalid: config.Load already
// validates every value, so an invalid Config is a programming error.
func New(store Store, checker Checker, cfg Config, log *slog.Logger) *Scheduler {
	if err := cfg.validate(); err != nil {
		panic("scheduler: invalid config: " + err.Error())
	}
	return &Scheduler{
		store:   store,
		checker: checker,
		cfg:     cfg,
		log:     log.With(slog.String("worker_id", cfg.WorkerID)),
		tokens:  make(chan struct{}, cfg.Concurrency),
	}
}

// Run claims once immediately and then once every PollInterval until ctx is
// cancelled. It then stops claiming, gives in-flight checks ShutdownTimeout
// to finish, cancels the ones still running (their leases are released, not
// recorded), and returns once every check goroutine has exited.
func (s *Scheduler) Run(ctx context.Context) {
	// Checks run under a context of their own so that the signal does not
	// abort them; only the shutdown timeout below cancels it.
	checkCtx, cancelChecks := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelChecks()

	var wg sync.WaitGroup

	s.log.Info("scheduler started",
		slog.Int("concurrency", s.cfg.Concurrency),
		slog.Int("batch_size", s.cfg.BatchSize),
		slog.Duration("poll_interval", s.cfg.PollInterval),
		slog.Duration("lease_duration", s.cfg.LeaseDuration),
	)

	ticker := time.NewTicker(s.cfg.PollInterval)
	s.poll(ctx, checkCtx, &wg)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			s.poll(ctx, checkCtx, &wg)
		}
	}
	ticker.Stop()

	s.drain(&wg, cancelChecks)
}

// drain waits for in-flight checks, cancelling them after ShutdownTimeout.
func (s *Scheduler) drain(wg *sync.WaitGroup, cancelChecks context.CancelFunc) {
	inFlight := len(s.tokens)
	s.log.Info("scheduler stopping",
		slog.Int("in_flight", inFlight),
		slog.Duration("shutdown_timeout", s.cfg.ShutdownTimeout))

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		s.log.Warn("shutdown timeout reached, cancelling in-flight checks",
			slog.Int("in_flight", len(s.tokens)))
		cancelChecks()
		<-done
	}
	s.log.Info("scheduler stopped")
}

// poll claims as many monitors as there are free slots, up to BatchSize, and
// starts one goroutine per claimed monitor.
func (s *Scheduler) poll(ctx, checkCtx context.Context, wg *sync.WaitGroup) {
	free := cap(s.tokens) - len(s.tokens)
	if free == 0 {
		s.log.Debug("all check slots busy, skipping claim")
		return
	}
	batch := min(s.cfg.BatchSize, free)

	monitors, err := s.store.Claim(ctx, s.cfg.WorkerID, int32(batch), s.cfg.LeaseDuration) //nolint:gosec // G115: batch <= WORKER_BATCH_SIZE, at most 500.
	if err != nil {
		if ctx.Err() != nil {
			// The signal arrived mid-claim; the claim rolled back.
			return
		}
		s.log.Error("claim failed, retrying next tick", slog.Any("error", err))
		return
	}
	if len(monitors) > 0 {
		s.log.Debug("claimed monitors", slog.Int("count", len(monitors)), slog.Int("requested", batch))
	}

	for _, m := range monitors {
		// Never blocks: only this loop acquires, and it asked for at most
		// the number of free slots.
		s.tokens <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-s.tokens }()
			s.check(ctx, checkCtx, m)
		}()
	}
}

// check runs one check and then records its outcome, or releases the lease if
// the check was cancelled.
//
// Monitor URLs and error messages are not logged: a URL may carry a secret in
// its query string (Rule 10). The monitor id identifies the monitor.
func (s *Scheduler) check(ctx, checkCtx context.Context, m domain.Monitor) {
	log := s.log.With(slog.String("monitor_id", m.ID.String()))

	outcome, checkErr := s.checker.Check(checkCtx, m)

	// Detached from the signal context so a write made after the signal
	// still completes, but bounded so shutdown cannot hang on the database.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
	defer cancel()

	if checkErr != nil {
		log.Info("check cancelled, releasing lease", slog.Any("error", checkErr))
		if err := s.store.Release(writeCtx, s.cfg.WorkerID, m.ID); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				log.Warn("lease lost before release")
				return
			}
			log.Error("release lease failed", slog.Any("error", err))
		}
		return
	}

	err := s.store.Record(writeCtx, s.cfg.WorkerID, m.ID, m.NextCheckAt, outcome)
	switch {
	case errors.Is(err, ErrLeaseLost):
		log.Warn("lease lost, check result discarded")
	case err != nil:
		log.Error("record check result failed", slog.Any("error", err))
	default:
		attrs := []any{
			slog.Bool("success", outcome.Success),
			slog.Int("latency_ms", int(outcome.LatencyMS)),
		}
		if outcome.StatusCode != nil {
			attrs = append(attrs, slog.Int("status_code", int(*outcome.StatusCode)))
		}
		if outcome.ErrorKind != nil {
			attrs = append(attrs, slog.String("error_kind", outcome.ErrorKind.String()))
		}
		log.Debug("check recorded", attrs...)
	}
}
