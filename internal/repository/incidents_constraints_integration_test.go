//go:build integration

package repository_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The tests in this file prove the incidents schema on its own, below the
// repository: DoD 3 says the database, not the application, guarantees at
// most one unresolved incident per monitor.

const (
	uniqueViolation       = "23505"
	oneUnresolvedIndex    = "incidents_one_unresolved_per_monitor"
	insertOpenIncidentSQL = `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`
)

// insertIncident writes an incident row directly on the shared pool.
// resolvedAt nil leaves it unresolved.
func insertIncident(t *testing.T, monitorID uuid.UUID, startedAt time.Time, resolvedAt *time.Time) error {
	t.Helper()

	_, err := sharedPool.Exec(t.Context(),
		`INSERT INTO incidents (monitor_id, started_at, resolved_at) VALUES ($1, $2, $3)`,
		monitorID, startedAt, resolvedAt)
	return err
}

// requireUniqueViolation fails unless err is SQLSTATE 23505 on the
// one-unresolved-per-monitor index.
func requireUniqueViolation(t *testing.T, err error) {
	t.Helper()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want a PostgreSQL error", err)
	}
	if pgErr.Code != uniqueViolation || pgErr.ConstraintName != oneUnresolvedIndex {
		t.Fatalf("error = %s on %q, want %s on %q", pgErr.Code, pgErr.ConstraintName, uniqueViolation, oneUnresolvedIndex)
	}
}

func countIncidents(t *testing.T, monitorID uuid.UUID, unresolvedOnly bool) int {
	t.Helper()

	var n int
	if err := sharedPool.QueryRow(t.Context(), `
		SELECT count(*) FROM incidents
		WHERE monitor_id = $1 AND (NOT $2 OR resolved_at IS NULL)`,
		monitorID, unresolvedOnly).Scan(&n); err != nil {
		t.Fatalf("counting incidents failed: %v", err)
	}
	return n
}

// TestIncidentsOneUnresolvedPerMonitor [DoD 3]: the partial unique index
// rejects a second unresolved incident for a monitor, and nothing else.
func TestIncidentsOneUnresolvedPerMonitor(t *testing.T) {
	truncateMonitors(t)
	ids := seedMonitors(t, dueSpec(2))
	monitorA, monitorB := ids[0], ids[1]

	start := dbNow(t).Add(-time.Hour)
	resolved := start.Add(10 * time.Minute)

	// Two resolved incidents for one monitor are fine.
	for i := range 2 {
		if err := insertIncident(t, monitorA, start.Add(time.Duration(i)*time.Second), &resolved); err != nil {
			t.Fatalf("resolved incident %d was rejected: %v", i, err)
		}
	}
	// So is one unresolved incident alongside them.
	if err := insertIncident(t, monitorA, start, nil); err != nil {
		t.Fatalf("unresolved incident next to resolved ones was rejected: %v", err)
	}

	// A second unresolved incident for the same monitor is not.
	requireUniqueViolation(t, insertIncident(t, monitorA, start.Add(time.Minute), nil))

	// Another monitor is unaffected and can hold its own open incident.
	if err := insertIncident(t, monitorB, start, nil); err != nil {
		t.Fatalf("unresolved incident for another monitor was rejected: %v", err)
	}

	if got := countIncidents(t, monitorA, true); got != 1 {
		t.Errorf("monitor A has %d unresolved incidents, want 1", got)
	}
	if got := countIncidents(t, monitorA, false); got != 3 {
		t.Errorf("monitor A has %d incidents, want 3", got)
	}
	if got := countIncidents(t, monitorB, true); got != 1 {
		t.Errorf("monitor B has %d unresolved incidents, want 1", got)
	}

	// Resolving the open incident frees the slot for a new one.
	if _, err := sharedPool.Exec(t.Context(),
		`UPDATE incidents SET resolved_at = now() WHERE monitor_id = $1 AND resolved_at IS NULL`, monitorA); err != nil {
		t.Fatalf("resolving the open incident failed: %v", err)
	}
	if err := insertIncident(t, monitorA, dbNow(t), nil); err != nil {
		t.Fatalf("unresolved incident after resolving the previous one was rejected: %v", err)
	}
}

// TestIncidentsConcurrentUnresolvedInsert [DoD 3]: two transactions that both
// insert an unresolved incident for one monitor while both are open — exactly
// one commits. The second insert waits on the first's uncommitted index entry
// and fails once it commits, so no interleaving yields two open incidents.
func TestIncidentsConcurrentUnresolvedInsert(t *testing.T) {
	truncateMonitors(t)
	monitorID := seedMonitors(t, dueSpec(1))[0]

	ctx := t.Context()
	txA, err := dedicatedConn(t).Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()
	txB, err := dedicatedConn(t).Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(context.Background()) }()

	if _, err := txA.Exec(ctx, insertOpenIncidentSQL, monitorID); err != nil {
		t.Fatalf("insert in A: %v", err)
	}

	insertB := make(chan error, 1)
	go func() {
		_, err := txB.Exec(ctx, insertOpenIncidentSQL, monitorID)
		insertB <- err
	}()

	// B must wait on A's uncommitted entry rather than succeed or fail early.
	select {
	case err := <-insertB:
		t.Fatalf("insert in B returned %v while A was still open, want it to block", err)
	case <-time.After(200 * time.Millisecond):
	}

	commitA := txA.Commit(ctx)
	errB := <-insertB
	commitB := error(nil)
	if errB == nil {
		commitB = txB.Commit(ctx)
	}

	committed := 0
	if commitA == nil {
		committed++
	}
	if errB == nil && commitB == nil {
		committed++
	}
	if committed != 1 {
		t.Fatalf("%d transactions committed (A commit: %v, B insert: %v, B commit: %v), want exactly 1",
			committed, commitA, errB, commitB)
	}
	if commitA != nil {
		t.Fatalf("A, which inserted first, failed to commit: %v", commitA)
	}
	requireUniqueViolation(t, errB)

	if got := countIncidents(t, monitorID, true); got != 1 {
		t.Errorf("%d unresolved incidents after the race, want 1", got)
	}
}

// TestIncidentsConstraints checks every CHECK constraint on incidents, so a
// bad row cannot land even if the repository is wrong.
func TestIncidentsConstraints(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)
	migrateUp(t, sqlDB)

	var monitorID string
	if err := sqlDB.QueryRowContext(t.Context(), `
		INSERT INTO monitors (name, url, interval_seconds, timeout_ms,
		                      expected_status_min, expected_status_max, failure_threshold)
		VALUES ('example', 'https://example.com', 60, 5000, 200, 399, 3)
		RETURNING id`).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}
	var checkID int64
	if err := sqlDB.QueryRowContext(t.Context(), `
		INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms)
		VALUES ($1, true, 200, 42)
		RETURNING id`, monitorID).Scan(&checkID); err != nil {
		t.Fatalf("insert check: %v", err)
	}

	// Every row is resolved, so the partial unique index never interferes.
	const insert = `
		INSERT INTO incidents (monitor_id, started_at, resolved_at,
		                       opening_check_id, closing_check_id, cause)
		VALUES ($1, $2, $3, $4, $5, $6)`

	type row struct {
		startedAt      time.Time
		resolvedAt     *time.Time
		openingCheckID *int64
		closingCheckID *int64
		cause          *string
	}

	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	end := start.Add(time.Minute)
	str := func(s string) *string { return &s }

	valid := row{startedAt: start, resolvedAt: &end, openingCheckID: &checkID, closingCheckID: &checkID, cause: str("timeout")}

	exec := func(t *testing.T, r row) error {
		t.Helper()
		_, err := sqlDB.ExecContext(t.Context(), insert, monitorID, r.startedAt, r.resolvedAt,
			r.openingCheckID, r.closingCheckID, r.cause)
		return err
	}

	// The valid rows must be accepted, otherwise every rejection below is
	// meaningless. They prove the bounds inclusive: a zero-length incident and
	// a cause of exactly the maximum length.
	instant := valid
	instant.resolvedAt = &start
	longest := valid
	longest.cause = str(strings.Repeat("x", 512))
	for name, r := range map[string]row{"resolved": valid, "zero length": instant, "longest cause": longest} {
		if err := exec(t, r); err != nil {
			t.Fatalf("valid %s incident was rejected: %v", name, err)
		}
	}

	before := start.Add(-time.Second)
	tests := []struct {
		name       string
		mutate     func(*row)
		constraint string
	}{
		{name: "resolved before start", mutate: func(r *row) { r.resolvedAt = &before }, constraint: "incidents_resolved_after_start"},
		{name: "closing check while unresolved", mutate: func(r *row) { r.resolvedAt = nil }, constraint: "incidents_closing_only_when_resolved"},
		{name: "cause too long", mutate: func(r *row) { r.cause = str(strings.Repeat("x", 513)) }, constraint: "incidents_cause_length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)

			err := exec(t, r)
			if err == nil {
				t.Fatalf("insert succeeded, want a violation of %s", tt.constraint)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("error = %v, want a violation of %s", err, tt.constraint)
			}
		})
	}
}

// TestIncidentsForeignKeys checks the delete behaviour retention (D8) and
// monitor deletion rely on: a hard-deleted monitor takes its incidents with
// it, and deleting a check an incident points at keeps the incident and sets
// the reference to NULL.
func TestIncidentsForeignKeys(t *testing.T) {
	truncateMonitors(t)
	ids := seedMonitors(t, dueSpec(2))
	kept, deleted := ids[0], ids[1]

	now := dbNow(t)
	openingID := insertCheck(t, kept, now.Add(-2*time.Minute), false)
	closingID := insertCheck(t, kept, now.Add(-time.Minute), true)

	var incidentID uuid.UUID
	if err := sharedPool.QueryRow(t.Context(), `
		INSERT INTO incidents (monitor_id, started_at, resolved_at, opening_check_id, closing_check_id, cause)
		VALUES ($1, $2, $3, $4, $5, 'unexpected_status')
		RETURNING id`,
		kept, now.Add(-2*time.Minute), now.Add(-time.Minute), openingID, closingID).Scan(&incidentID); err != nil {
		t.Fatalf("inserting the referencing incident failed: %v", err)
	}

	readRefs := func() (opening, closing *int64) {
		t.Helper()
		if err := sharedPool.QueryRow(t.Context(),
			`SELECT opening_check_id, closing_check_id FROM incidents WHERE id = $1`, incidentID).
			Scan(&opening, &closing); err != nil {
			t.Fatalf("reading the incident failed: %v", err)
		}
		return opening, closing
	}

	t.Run("deleting a referenced check sets the reference NULL", func(t *testing.T) {
		if _, err := sharedPool.Exec(t.Context(), `DELETE FROM monitor_checks WHERE id = $1`, openingID); err != nil {
			t.Fatalf("deleting the opening check failed: %v", err)
		}
		opening, closing := readRefs()
		if opening != nil {
			t.Errorf("opening_check_id = %d after its check was deleted, want NULL", *opening)
		}
		if closing == nil || *closing != closingID {
			t.Errorf("closing_check_id = %v, want %d (untouched)", deref(closing), closingID)
		}

		if _, err := sharedPool.Exec(t.Context(), `DELETE FROM monitor_checks WHERE id = $1`, closingID); err != nil {
			t.Fatalf("deleting the closing check failed: %v", err)
		}
		if _, closing := readRefs(); closing != nil {
			t.Errorf("closing_check_id = %d after its check was deleted, want NULL", *closing)
		}
	})

	t.Run("deleting a monitor cascades to its incidents", func(t *testing.T) {
		if err := insertIncident(t, deleted, now, nil); err != nil {
			t.Fatalf("inserting an incident failed: %v", err)
		}
		if _, err := sharedPool.Exec(t.Context(), `DELETE FROM monitors WHERE id = $1`, deleted); err != nil {
			t.Fatalf("deleting the monitor failed: %v", err)
		}
		if got := countIncidents(t, deleted, false); got != 0 {
			t.Errorf("%d incidents remain for the deleted monitor, want 0", got)
		}
		// The other monitor's incident is untouched.
		if got := countIncidents(t, kept, false); got != 1 {
			t.Errorf("%d incidents for the other monitor, want 1", got)
		}
	})
}
