package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Retention settings (Sprint 4 D8, D9). They are code constants on purpose,
// not environment variables; only the retention period itself
// (CHECK_RETENTION_DAYS) is configurable.
const (
	// RetentionInterval is the time between cleanup runs. The first run
	// happens at startup.
	RetentionInterval = time.Hour
	// RetentionBatchSize is the most check rows one DELETE removes. Small
	// batches keep each statement's locks and WAL burst short; a run keeps
	// deleting batches until one comes back short.
	RetentionBatchSize int32 = 5000
)

// RetentionStore deletes expired check history. It is implemented by
// repository.RetentionStore.
type RetentionStore interface {
	// DeleteExpiredChecks deletes up to batchSize monitor_checks rows older
	// than retentionDays, oldest first, and returns how many it deleted.
	// Rows locked by a concurrent cleaner are skipped, so several workers
	// may run it at once.
	DeleteExpiredChecks(ctx context.Context, retentionDays, batchSize int32) (int64, error)
}

// Retention periodically deletes check rows older than the retention period.
// Incidents that reference a deleted check keep their row; the database sets
// the reference to NULL.
type Retention struct {
	store         RetentionStore
	retentionDays int32
	log           *slog.Logger
	// newTicker returns the channel that triggers runs after the first one
	// and a function that stops it. Production uses a RetentionInterval
	// time.Ticker; tests drive the channel by hand.
	newTicker func() (<-chan time.Time, func())
}

// NewRetention builds a runner that keeps retentionDays days of check
// history. It panics if retentionDays is out of range: config.Load already
// validates CHECK_RETENTION_DAYS (1..365), so that is a programming error.
func NewRetention(store RetentionStore, retentionDays int, log *slog.Logger) *Retention {
	if retentionDays < 1 || retentionDays > 365 {
		panic(fmt.Sprintf("scheduler: retention days %d outside 1..365", retentionDays))
	}
	return &Retention{
		store:         store,
		retentionDays: int32(retentionDays), // bounded to 1..365 above
		log:           log,
		newTicker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(RetentionInterval)
			return t.C, t.Stop
		},
	}
}

// Run cleans up once immediately and then once every RetentionInterval until
// ctx is cancelled. A failed run is logged and retried on the next tick. Run
// returns once ctx is done; a DELETE in flight at that moment is cancelled
// and rolls back, which is harmless because the next run deletes it again.
func (r *Retention) Run(ctx context.Context) {
	ticks, stop := r.newTicker()
	defer stop()

	r.log.Info("retention started",
		slog.Int("retention_days", int(r.retentionDays)),
		slog.Int("batch_size", int(RetentionBatchSize)),
		slog.Duration("interval", RetentionInterval))

	r.cleanup(ctx)
	for {
		select {
		case <-ctx.Done():
			r.log.Info("retention stopped")
			return
		case <-ticks:
			r.cleanup(ctx)
		}
	}
}

// cleanup deletes expired checks in batches of RetentionBatchSize until a
// batch comes back short, then logs the total.
func (r *Retention) cleanup(ctx context.Context) {
	start := time.Now()
	var total int64
	for {
		n, err := r.store.DeleteExpiredChecks(ctx, r.retentionDays, RetentionBatchSize)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown arrived mid-batch; that batch rolled back.
				r.log.Info("retention cleanup interrupted by shutdown", slog.Int64("deleted", total))
				return
			}
			r.log.Error("retention cleanup failed, retrying next tick",
				slog.Int64("deleted", total), slog.Any("error", err))
			return
		}
		total += n
		if n < int64(RetentionBatchSize) {
			break
		}
		if ctx.Err() != nil {
			r.log.Info("retention cleanup interrupted by shutdown", slog.Int64("deleted", total))
			return
		}
	}
	r.log.Info("retention cleanup finished",
		slog.Int64("deleted", total),
		slog.Int("retention_days", int(r.retentionDays)),
		slog.Duration("duration", time.Since(start)))
}
