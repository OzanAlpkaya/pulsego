package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

// SchedulerStore implements the scheduler's lease lifecycle — claim, record,
// release — on top of the generated queries. Every time it writes comes from
// PostgreSQL's now(), never from the worker's clock.
type SchedulerStore struct {
	q *sqlcgen.Queries
}

// NewSchedulerStore builds a store over a connection pool. It accepts any
// sqlcgen.DBTX — a *pgxpool.Pool in production — so that a claim can also run
// on a single connection or inside a transaction, which is how the integration
// tests hold row locks open.
func NewSchedulerStore(db sqlcgen.DBTX) *SchedulerStore {
	return &SchedulerStore{q: sqlcgen.New(db)}
}

// Claim leases up to batchSize due monitors to workerID in one statement, so
// the lease commits before any network call. Rows locked by a concurrent
// claimer are skipped, never waited on. Each lease lasts
// GREATEST(lease, monitor timeout + 5s).
//
// Each returned monitor's NextCheckAt is the claimed next_check_at that Record
// needs to tell a routine reschedule from a manual request made mid-check.
func (s *SchedulerStore) Claim(ctx context.Context, workerID string, batchSize int32, lease time.Duration) ([]domain.Monitor, error) {
	rows, err := s.q.ClaimDueMonitors(ctx, sqlcgen.ClaimDueMonitorsParams{
		WorkerID:     &workerID,
		LeaseSeconds: lease.Seconds(),
		BatchSize:    batchSize,
	})
	if err != nil {
		return nil, translateError(err, "claim due monitors")
	}

	monitors := make([]domain.Monitor, 0, len(rows))
	for _, row := range rows {
		monitors = append(monitors, toDomain(row))
	}
	return monitors, nil
}

// Record stores a check result and releases the lease in one statement.
// claimedNextCheckAt is the NextCheckAt that Claim returned for the monitor.
//
// If workerID no longer holds the lease — it expired and another worker
// reclaimed the monitor, or the monitor was soft-deleted — nothing is written
// and Record returns scheduler.ErrLeaseLost.
func (s *SchedulerStore) Record(
	ctx context.Context,
	workerID string,
	monitorID uuid.UUID,
	claimedNextCheckAt time.Time,
	outcome domain.CheckOutcome,
) error {
	var errorKind *string
	if outcome.ErrorKind != nil {
		kind := outcome.ErrorKind.String()
		errorKind = &kind
	}

	_, err := s.q.RecordCheckResult(ctx, sqlcgen.RecordCheckResultParams{
		Success:            outcome.Success,
		StatusCode:         outcome.StatusCode,
		LatencyMs:          outcome.LatencyMS,
		ErrorKind:          errorKind,
		ErrorMessage:       outcome.ErrorMessage,
		ClaimedNextCheckAt: claimedNextCheckAt,
		MonitorID:          monitorID,
		WorkerID:           &workerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return scheduler.ErrLeaseLost
	}
	if err != nil {
		return translateError(err, "record check result")
	}
	return nil
}

// Release gives a lease back without recording a result, for example when a
// check is cancelled at shutdown. next_check_at is untouched, so the monitor
// stays due. Releasing a lease workerID does not hold returns
// scheduler.ErrLeaseLost.
func (s *SchedulerStore) Release(ctx context.Context, workerID string, monitorID uuid.UUID) error {
	n, err := s.q.ReleaseLease(ctx, sqlcgen.ReleaseLeaseParams{
		MonitorID: monitorID,
		WorkerID:  &workerID,
	})
	if err != nil {
		return translateError(err, "release lease")
	}
	if n == 0 {
		return scheduler.ErrLeaseLost
	}
	return nil
}
