package repository

import (
	"context"
	"errors"
	"fmt"
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
	db TxBeginner
	q  *sqlcgen.Queries
}

// TxBeginner is a sqlcgen.DBTX that can also start a transaction, which
// Record needs. *pgxpool.Pool, *pgx.Conn and pgx.Tx (as a savepoint) all
// satisfy it.
type TxBeginner interface {
	sqlcgen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewSchedulerStore builds a store over a connection pool — a *pgxpool.Pool in
// production — or over a single connection or transaction, which is how the
// integration tests hold row locks open.
func NewSchedulerStore(db TxBeginner) *SchedulerStore {
	return &SchedulerStore{db: db, q: sqlcgen.New(db)}
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

// Record stores a check result, applies the resulting state transition,
// opens or resolves an incident, and releases the lease, all in one
// transaction (Sprint 4 D3). claimedNextCheckAt is the NextCheckAt that Claim
// returned for the monitor.
//
// The transaction is:
//  1. BEGIN (READ COMMITTED).
//  2. Lock the leased monitor row FOR UPDATE.
//  3. Compute domain.Transition from the locked row — never from the
//     claim-time snapshot, which may be stale.
//  4. Insert the monitor_checks row.
//  5. Write state, consecutive_failures and last_* to the monitor, reschedule
//     it, and release the lease.
//  6. Open (IncidentOpen) or resolve (IncidentResolve) the incident.
//  7. COMMIT.
//
// Race safety: every writer of a result holds the monitor row lock from step
// 2 until commit, so results for one monitor serialize. The lease guard in
// step 2 means a second, concurrent recorder finds no row once the first
// commits (the lease is released in step 5). The partial unique index
// incidents_one_unresolved_per_monitor is the final guarantee that a monitor
// never has two open incidents.
//
// If workerID no longer holds the lease — it expired and another worker
// reclaimed the monitor, or the monitor was soft-deleted — nothing is written
// and Record returns scheduler.ErrLeaseLost. On any other error the
// transaction rolls back, nothing persists, and the lease stays held until it
// expires.
//
// The returned change has IncidentAlreadyOpen set when the transition asked
// for an incident to be opened but one was already open (OpenIncident
// affected no row). The result is still recorded; the caller should warn.
func (s *SchedulerStore) Record(
	ctx context.Context,
	workerID string,
	monitorID uuid.UUID,
	claimedNextCheckAt time.Time,
	outcome domain.CheckOutcome,
) (change domain.StateChange, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return domain.StateChange{}, translateError(err, "begin record transaction")
	}
	defer func() {
		if err != nil {
			// The rollback error adds nothing: the transaction is discarded
			// either way, and err already says why.
			_ = tx.Rollback(ctx)
		}
	}()
	q := s.q.WithTx(tx)

	// Step 2. The row lock is held until commit.
	locked, err := q.LockLeasedMonitor(ctx, sqlcgen.LockLeasedMonitorParams{
		MonitorID: monitorID,
		WorkerID:  &workerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.StateChange{}, scheduler.ErrLeaseLost
	}
	if err != nil {
		return domain.StateChange{}, translateError(err, "lock leased monitor")
	}

	// Step 3.
	change, err = domain.Transition(domain.State(locked.State), locked.ConsecutiveFailures,
		locked.FailureThreshold, outcome.Success)
	if err != nil {
		return domain.StateChange{}, fmt.Errorf("compute state transition: %w", err)
	}

	// Step 4.
	var errorKind *string
	if outcome.ErrorKind != nil {
		kind := outcome.ErrorKind.String()
		errorKind = &kind
	}
	checkID, err := q.InsertCheck(ctx, sqlcgen.InsertCheckParams{
		MonitorID:    monitorID,
		Success:      outcome.Success,
		StatusCode:   outcome.StatusCode,
		LatencyMs:    outcome.LatencyMS,
		ErrorKind:    errorKind,
		ErrorMessage: outcome.ErrorMessage,
	})
	if err != nil {
		return domain.StateChange{}, translateError(err, "insert check")
	}

	// Step 5.
	if err = q.ApplyCheckResult(ctx, sqlcgen.ApplyCheckResultParams{
		State:               string(change.To),
		ConsecutiveFailures: change.ConsecutiveFailures,
		StatusCode:          outcome.StatusCode,
		LatencyMs:           outcome.LatencyMS,
		ClaimedNextCheckAt:  claimedNextCheckAt,
		MonitorID:           monitorID,
	}); err != nil {
		return domain.StateChange{}, translateError(err, "apply check result")
	}

	// Step 6.
	switch change.Incident {
	case domain.IncidentOpen:
		var opened int64
		opened, err = q.OpenIncident(ctx, sqlcgen.OpenIncidentParams{
			MonitorID:      monitorID,
			OpeningCheckID: checkID,
			Cause:          incidentCause(outcome),
		})
		if err != nil {
			return domain.StateChange{}, translateError(err, "open incident")
		}
		change.IncidentAlreadyOpen = opened == 0
	case domain.IncidentResolve:
		// 0 rows (nothing was open) is not an error.
		if _, err = q.ResolveIncident(ctx, sqlcgen.ResolveIncidentParams{
			ClosingCheckID: checkID,
			MonitorID:      monitorID,
		}); err != nil {
			return domain.StateChange{}, translateError(err, "resolve incident")
		}
	case domain.IncidentNone:
	}

	// Step 7.
	if err = tx.Commit(ctx); err != nil {
		return domain.StateChange{}, translateError(err, "commit check result")
	}
	return change, nil
}

// incidentCause renders an incident's cause from its opening check. A failed
// outcome always carries an error kind (monitor_checks_outcome_consistent);
// unknown is only a fallback.
func incidentCause(outcome domain.CheckOutcome) string {
	kind := domain.ErrorKindUnknown
	if outcome.ErrorKind != nil {
		kind = *outcome.ErrorKind
	}
	return domain.IncidentCause(kind, outcome.ErrorMessage)
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
