package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
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
	// Record stores a check result, applies the state transition, opens or
	// resolves an incident, and releases the lease, atomically. It returns
	// the applied change, with IncidentAlreadyOpen set if an incident should
	// have been opened but one already was. It returns ErrLeaseLost if
	// workerID no longer holds the lease.
	Record(ctx context.Context, workerID string, monitorID uuid.UUID,
		claimedNextCheckAt time.Time, outcome domain.CheckOutcome) (domain.StateChange, error)
	// Release gives a lease back without a result; the monitor stays due.
	Release(ctx context.Context, workerID string, monitorID uuid.UUID) error
}

// Checker runs one check. It is implemented by checker.Checker.
type Checker interface {
	// Check returns a non-nil error only when ctx was cancelled; that is not
	// a check result and is never recorded.
	Check(ctx context.Context, m domain.Monitor) (domain.CheckOutcome, error)
}

// Metrics is what the scheduler records (Sprint 5 D3, metrics 3–9). It is
// implemented by *telemetry.Metrics, which bounds every label itself; the
// scheduler passes only domain enums and durations, never a URL, monitor id,
// error message or worker id.
type Metrics interface {
	// ObserveCheck records one finished check: its result, error kind and
	// duration.
	ObserveCheck(success bool, kind *domain.ErrorKind, d time.Duration)
	// StateChanged records a recorded result that changed the state.
	StateChanged(from, to domain.State)
	// AddClaimed records n claimed monitors.
	AddClaimed(n int)
	// ClaimError records one failed claim.
	ClaimError()
	// ActiveChecksInc and ActiveChecksDec bracket every running check.
	ActiveChecksInc()
	ActiveChecksDec()
	// ObserveSchedulerLag records claim time minus a claimed monitor's
	// next_check_at.
	ObserveSchedulerLag(d time.Duration)
}

var _ Metrics = (*telemetry.Metrics)(nil)

// noopMetrics is used when Config.Metrics is nil.
type noopMetrics struct{}

func (noopMetrics) ObserveCheck(bool, *domain.ErrorKind, time.Duration) {}
func (noopMetrics) StateChanged(domain.State, domain.State)             {}
func (noopMetrics) AddClaimed(int)                                      {}
func (noopMetrics) ClaimError()                                         {}
func (noopMetrics) ActiveChecksInc()                                    {}
func (noopMetrics) ActiveChecksDec()                                    {}
func (noopMetrics) ObserveSchedulerLag(time.Duration)                   {}

// Span names and attributes of the worker traces (Sprint 5 D4).
const (
	spanClaim  = "scheduler.claim"
	spanCheck  = "scheduler.check"
	spanRecord = "monitor.record"

	attrClaimed   = attribute.Key("pulsego.claimed")
	attrRequested = attribute.Key("pulsego.requested")
	attrMonitorID = attribute.Key("pulsego.monitor.id")
	attrSuccess   = attribute.Key("pulsego.check.success")
	attrErrorKind = attribute.Key("pulsego.error_kind")
	attrStateFrom = attribute.Key("state.from")
	attrStateTo   = attribute.Key("state.to")

	eventIncidentOpened   = "incident.opened"
	eventIncidentResolved = "incident.resolved"
)

// Config holds the scheduler's settings. Every field is required unless
// marked optional; the values come from the validated config.Worker and
// config.Config.ShutdownTimeout.
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
	// Metrics is optional; nil records nothing.
	Metrics Metrics
	// TracerProvider is optional; nil uses the global provider at span
	// time, so spans follow whatever telemetry.SetupTracing installed.
	TracerProvider trace.TracerProvider
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
	metrics Metrics
	tracer  trace.TracerProvider
}

// New builds a Scheduler. It panics if cfg is invalid: config.Load already
// validates every value, so an invalid Config is a programming error.
func New(store Store, checker Checker, cfg Config, log *slog.Logger) *Scheduler {
	if err := cfg.validate(); err != nil {
		panic("scheduler: invalid config: " + err.Error())
	}
	var metrics Metrics = noopMetrics{}
	if cfg.Metrics != nil {
		metrics = cfg.Metrics
	}
	return &Scheduler{
		store:   store,
		checker: checker,
		cfg:     cfg,
		log:     log.With(slog.String("worker_id", cfg.WorkerID)),
		tokens:  make(chan struct{}, cfg.Concurrency),
		metrics: metrics,
		tracer:  cfg.TracerProvider,
	}
}

// startSpan starts a span on the configured provider, or the global one.
func (s *Scheduler) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return telemetry.TracerFrom(s.tracer).Start(ctx, name, opts...)
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

	monitors, link, ok := s.claim(ctx, batch)
	if !ok {
		return
	}

	for _, m := range monitors {
		// Never blocks: only this loop acquires, and it asked for at most
		// the number of free slots.
		s.tokens <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-s.tokens }()
			s.metrics.ActiveChecksInc()
			defer s.metrics.ActiveChecksDec()
			s.check(ctx, checkCtx, m, link)
		}()
	}
}

// claim runs one claim under a scheduler.claim span and records its metrics.
// It returns the claimed monitors, a link to the claim span for the check
// spans, and false if the claim failed.
func (s *Scheduler) claim(ctx context.Context, batch int) ([]domain.Monitor, trace.Link, bool) {
	spanCtx, span := s.startSpan(ctx, spanClaim, trace.WithAttributes(attrRequested.Int(batch)))
	defer span.End()

	monitors, err := s.store.Claim(spanCtx, s.cfg.WorkerID, int32(batch), s.cfg.LeaseDuration) //nolint:gosec // G115: batch <= WORKER_BATCH_SIZE, at most 500.
	if err != nil {
		if ctx.Err() != nil {
			// The signal arrived mid-claim; the claim rolled back. That is
			// shutdown, not a claim error.
			span.SetStatus(codes.Error, "claim cancelled")
			return nil, trace.Link{}, false
		}
		s.metrics.ClaimError()
		// The driver error may quote values; the span gets a fixed text.
		span.SetStatus(codes.Error, "claim failed")
		s.log.Error("claim failed, retrying next tick", slog.Any("error", err))
		return nil, trace.Link{}, false
	}

	claimedAt := time.Now()
	span.SetAttributes(attrClaimed.Int(len(monitors)))
	s.metrics.AddClaimed(len(monitors))
	for _, m := range monitors {
		// ObserveSchedulerLag clamps a negative lag (clock skew) to zero.
		s.metrics.ObserveSchedulerLag(max(claimedAt.Sub(m.NextCheckAt), 0))
	}
	if len(monitors) > 0 {
		s.log.Debug("claimed monitors", slog.Int("count", len(monitors)), slog.Int("requested", batch))
	}
	return monitors, trace.LinkFromContext(spanCtx), true
}

// check runs one check and then records its outcome, or releases the lease if
// the check was cancelled.
//
// Monitor URLs and error messages are not logged: a URL may carry a secret in
// its query string (Rule 10). The monitor id identifies the monitor.
//
// Each check is the root of its own trace (scheduler.check), linked to the
// claim that produced it: a claim batch can hold hundreds of monitors, and one
// trace per check keeps them readable. The checker and record spans, and the
// database spans below them, are its children.
func (s *Scheduler) check(ctx, checkCtx context.Context, m domain.Monitor, claim trace.Link) {
	log := s.log.With(slog.String("monitor_id", m.ID.String()))

	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(attrMonitorID.String(m.ID.String())),
	}
	if claim.SpanContext.IsValid() {
		opts = append(opts, trace.WithLinks(claim))
	}
	spanCtx, span := s.startSpan(checkCtx, spanCheck, opts...)
	defer span.End()

	outcome, checkErr := s.checker.Check(spanCtx, m)

	// Detached from the signal context so a write made after the signal
	// still completes, but bounded so shutdown cannot hang on the database.
	// It keeps the span, so the database spans nest under this check.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(spanCtx), WriteTimeout)
	defer cancel()

	if checkErr != nil {
		span.SetStatus(codes.Error, "check cancelled")
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

	// A check that produced a result counts, even if its record is later
	// discarded: the target was contacted either way.
	s.metrics.ObserveCheck(outcome.Success, outcome.ErrorKind, time.Duration(outcome.LatencyMS)*time.Millisecond)
	span.SetAttributes(attrSuccess.Bool(outcome.Success))
	if outcome.ErrorKind != nil {
		span.SetAttributes(attrErrorKind.String(outcome.ErrorKind.String()))
	}

	change, err := s.record(writeCtx, m, outcome)
	if err != nil {
		span.SetStatus(codes.Error, "record failed")
	}
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
		if change.From != change.To {
			s.metrics.StateChanged(change.From, change.To)
			log.Info("monitor state changed",
				slog.String("from", string(change.From)),
				slog.String("to", string(change.To)),
				slog.String("incident", change.Incident.String()))
		}
		if change.IncidentAlreadyOpen {
			log.Warn("incident already open, none opened",
				slog.String("from", string(change.From)),
				slog.String("to", string(change.To)))
		}
	}
}

// record stores the outcome under a monitor.record span carrying the state
// transition and an event for an opened or resolved incident.
func (s *Scheduler) record(ctx context.Context, m domain.Monitor, outcome domain.CheckOutcome) (domain.StateChange, error) {
	ctx, span := s.startSpan(ctx, spanRecord)
	defer span.End()

	change, err := s.store.Record(ctx, s.cfg.WorkerID, m.ID, m.NextCheckAt, outcome)
	if err != nil {
		// Fixed texts only: a driver error may quote values.
		if errors.Is(err, ErrLeaseLost) {
			span.SetStatus(codes.Error, "lease lost")
		} else {
			span.SetStatus(codes.Error, "record failed")
		}
		return change, err
	}

	span.SetAttributes(attrStateFrom.String(string(change.From)), attrStateTo.String(string(change.To)))
	switch change.Incident {
	case domain.IncidentOpen:
		if !change.IncidentAlreadyOpen {
			span.AddEvent(eventIncidentOpened)
		}
	case domain.IncidentResolve:
		span.AddEvent(eventIncidentResolved)
	case domain.IncidentNone:
	}
	return change, nil
}
