//go:build integration

package repository_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

// The tests in this file prove Sprint 4 D3: a check result, the monitor's
// state transition and its incident are recorded in one transaction that is
// race-safe for one monitor [DoD 2]. Seeded monitors have failure_threshold 3.

const (
	recordWorker    = "worker-record"
	seededThreshold = 3
)

// armLease gives workerID a fresh lease on the monitor, as a claim would, and
// returns the claimed next_check_at that Record expects.
func armLease(t *testing.T, id uuid.UUID, workerID string) time.Time {
	t.Helper()

	var next time.Time
	if err := sharedPool.QueryRow(t.Context(), `
		UPDATE monitors
		SET lease_owner = $2, lease_until = now() + interval '30 seconds'
		WHERE id = $1
		RETURNING next_check_at`, id, workerID).Scan(&next); err != nil {
		t.Fatalf("arming the lease failed: %v", err)
	}
	return next
}

// setState writes a monitor's state and failure count directly, as a hand
// edit or an earlier history would have left them.
func setState(t *testing.T, id uuid.UUID, state domain.State, failures int32) {
	t.Helper()

	if _, err := sharedPool.Exec(t.Context(),
		"UPDATE monitors SET state = $2, consecutive_failures = $3 WHERE id = $1",
		id, string(state), failures); err != nil {
		t.Fatalf("setting the monitor state failed: %v", err)
	}
}

func readState(t *testing.T, id uuid.UUID) (domain.State, int32) {
	t.Helper()

	var (
		state    string
		failures int32
	)
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT state, consecutive_failures FROM monitors WHERE id = $1", id).
		Scan(&state, &failures); err != nil {
		t.Fatalf("reading the monitor state failed: %v", err)
	}
	return domain.State(state), failures
}

// latestCheck returns the id and checked_at of the monitor's newest check.
func latestCheck(t *testing.T, id uuid.UUID) (int64, time.Time) {
	t.Helper()

	var (
		checkID   int64
		checkedAt time.Time
	)
	if err := sharedPool.QueryRow(t.Context(), `
		SELECT id, checked_at FROM monitor_checks
		WHERE monitor_id = $1 ORDER BY id DESC LIMIT 1`, id).
		Scan(&checkID, &checkedAt); err != nil {
		t.Fatalf("reading the latest check failed: %v", err)
	}
	return checkID, checkedAt
}

func listIncidents(t *testing.T, id uuid.UUID) []domain.Incident {
	t.Helper()

	rows, err := sharedPool.Query(t.Context(), `
		SELECT id, monitor_id, started_at, resolved_at, opening_check_id,
		       closing_check_id, cause, created_at
		FROM incidents WHERE monitor_id = $1
		ORDER BY started_at, created_at`, id)
	if err != nil {
		t.Fatalf("listing incidents failed: %v", err)
	}
	incidents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Incident, error) {
		var inc domain.Incident
		err := row.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt,
			&inc.OpeningCheckID, &inc.ClosingCheckID, &inc.Cause, &inc.CreatedAt)
		return inc, err
	})
	if err != nil {
		t.Fatalf("scanning incidents failed: %v", err)
	}
	return incidents
}

func failureOutcome() domain.CheckOutcome {
	return domain.CheckOutcome{
		Success:      false,
		LatencyMS:    9,
		ErrorKind:    ptr(domain.ErrorKindConnection),
		ErrorMessage: ptr("connection refused"),
	}
}

func randomOutcome(r *rand.Rand) domain.CheckOutcome {
	if r.IntN(2) == 0 {
		return successOutcome()
	}
	return failureOutcome()
}

// TestRecordLifecycleOpensAndResolvesIncident [DoD 2] walks a monitor through
// every incident-relevant edge and checks the incident rows at each step.
func TestRecordLifecycleOpensAndResolvesIncident(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	type step struct {
		name     string
		success  bool
		from, to domain.State
		failures int32
		incident domain.IncidentAction
	}
	steps := []step{
		{"unknown to up", true, domain.StateUnknown, domain.StateUp, 0, domain.IncidentNone},
		{"up to degraded", false, domain.StateUp, domain.StateDegraded, 1, domain.IncidentNone},
		{"degraded below threshold", false, domain.StateDegraded, domain.StateDegraded, 2, domain.IncidentNone},
		{"degraded to down at threshold", false, domain.StateDegraded, domain.StateDown, 3, domain.IncidentOpen},
		{"down to down", false, domain.StateDown, domain.StateDown, 4, domain.IncidentNone},
		{"down to up", true, domain.StateDown, domain.StateUp, 0, domain.IncidentResolve},
		{"up to degraded again", false, domain.StateUp, domain.StateDegraded, 1, domain.IncidentNone},
		{"degraded below threshold again", false, domain.StateDegraded, domain.StateDegraded, 2, domain.IncidentNone},
		{"degraded to down again", false, domain.StateDegraded, domain.StateDown, 3, domain.IncidentOpen},
	}

	var first domain.Incident
	for i, st := range steps {
		claimed := armLease(t, id, recordWorker)
		outcome := successOutcome()
		if !st.success {
			outcome = failureOutcome()
		}
		change, err := store.Record(t.Context(), recordWorker, id, claimed, outcome)
		if err != nil {
			t.Fatalf("step %d (%s): Record() returned %v", i, st.name, err)
		}
		want := domain.StateChange{From: st.from, To: st.to, ConsecutiveFailures: st.failures, Incident: st.incident}
		if change != want {
			t.Fatalf("step %d (%s): change = %+v, want %+v", i, st.name, change, want)
		}
		if state, failures := readState(t, id); state != st.to || failures != st.failures {
			t.Fatalf("step %d (%s): stored state = %s with %d failures, want %s with %d",
				i, st.name, state, failures, st.to, st.failures)
		}
		if n := countChecks(t, id); n != i+1 {
			t.Fatalf("step %d (%s): %d check rows, want %d", i, st.name, n, i+1)
		}

		checkID, checkedAt := latestCheck(t, id)
		incidents := listIncidents(t, id)
		switch i {
		case 0, 1, 2:
			if len(incidents) != 0 {
				t.Fatalf("step %d (%s): %d incidents before reaching down, want 0", i, st.name, len(incidents))
			}
		case 3:
			if len(incidents) != 1 {
				t.Fatalf("step %d (%s): %d incidents, want 1", i, st.name, len(incidents))
			}
			first = incidents[0]
			if first.ResolvedAt != nil {
				t.Errorf("new incident resolved_at = %v, want NULL", first.ResolvedAt)
			}
			if first.OpeningCheckID == nil || *first.OpeningCheckID != checkID {
				t.Errorf("opening_check_id = %v, want %d", deref(first.OpeningCheckID), checkID)
			}
			if !first.StartedAt.Equal(checkedAt) {
				t.Errorf("started_at = %v, want the opening check's checked_at %v", first.StartedAt, checkedAt)
			}
			if first.Cause == nil || *first.Cause != "connection: connection refused" {
				t.Errorf("cause = %v, want %q", deref(first.Cause), "connection: connection refused")
			}
		case 4:
			if len(incidents) != 1 || incidents[0].ResolvedAt != nil {
				t.Fatalf("step %d (%s): incidents = %+v, want the one open incident and no second", i, st.name, incidents)
			}
		case 5:
			if len(incidents) != 1 {
				t.Fatalf("step %d (%s): %d incidents, want 1", i, st.name, len(incidents))
			}
			inc := incidents[0]
			if inc.ID != first.ID {
				t.Errorf("resolved incident %s, want the open one %s", inc.ID, first.ID)
			}
			if inc.ResolvedAt == nil || !inc.ResolvedAt.Equal(checkedAt) {
				t.Errorf("resolved_at = %v, want the closing check's checked_at %v", deref(inc.ResolvedAt), checkedAt)
			}
			if inc.ClosingCheckID == nil || *inc.ClosingCheckID != checkID {
				t.Errorf("closing_check_id = %v, want %d", deref(inc.ClosingCheckID), checkID)
			}
		case 6, 7:
			if n := countIncidents(t, id, true); n != 0 {
				t.Fatalf("step %d (%s): %d unresolved incidents, want 0", i, st.name, n)
			}
		case 8:
			if len(incidents) != 2 {
				t.Fatalf("step %d (%s): %d incidents, want 2", i, st.name, len(incidents))
			}
			second := incidents[1]
			if second.ID == first.ID || second.ResolvedAt != nil {
				t.Errorf("second incident = %+v, want a new, open incident", second)
			}
			if second.OpeningCheckID == nil || *second.OpeningCheckID != checkID {
				t.Errorf("second opening_check_id = %v, want %d", deref(second.OpeningCheckID), checkID)
			}
		}
	}
}

// recordResult is what one concurrent recorder saw.
type recordResult struct {
	change domain.StateChange
	err    error
}

// recordConcurrently runs one Record per store at once, released together by a
// barrier, and returns their results.
func recordConcurrently(stores []*repository.SchedulerStore, id uuid.UUID, claimed time.Time,
	outcomes []domain.CheckOutcome,
) []recordResult {
	results := make([]recordResult, len(stores))
	var ready, done sync.WaitGroup
	barrier := make(chan struct{})
	for i, store := range stores {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-barrier
			change, err := store.Record(context.Background(), recordWorker, id, claimed, outcomes[i])
			results[i] = recordResult{change: change, err: err}
		}()
	}
	ready.Wait()
	close(barrier)
	done.Wait()
	return results
}

// TestRecordConcurrentResultsForOneMonitor [DoD 2]: concurrent recorders for
// one monitor serialize on its row lock; exactly one wins each round, and a
// monitor is down exactly when it has one unresolved incident.
func TestRecordConcurrentResultsForOneMonitor(t *testing.T) {
	truncateMonitors(t)
	id := seedMonitors(t, dueSpec(1))[0]
	setState(t, id, domain.StateDegraded, seededThreshold-1)

	// Each recorder has its own connection, so they really run in parallel.
	const racers = 16
	stores := make([]*repository.SchedulerStore, racers)
	for i := range stores {
		stores[i] = repository.NewSchedulerStore(dedicatedConn(t))
	}

	requireOneWinner := func(t *testing.T, round int, results []recordResult) recordResult {
		t.Helper()
		var winner recordResult
		wins := 0
		for _, r := range results {
			switch {
			case r.err == nil:
				wins++
				winner = r
			case !errors.Is(r.err, scheduler.ErrLeaseLost):
				t.Fatalf("round %d: Record() returned %v, want nil or ErrLeaseLost", round, r.err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d recorders succeeded, want exactly 1", round, wins)
		}
		return winner
	}

	// Round 0: 16 failures at threshold − 1.
	failures := make([]domain.CheckOutcome, racers)
	for i := range failures {
		failures[i] = failureOutcome()
	}
	claimed := armLease(t, id, recordWorker)
	winner := requireOneWinner(t, 0, recordConcurrently(stores, id, claimed, failures))
	if winner.change.To != domain.StateDown || winner.change.Incident != domain.IncidentOpen {
		t.Errorf("winning change = %+v, want degraded→down opening an incident", winner.change)
	}
	if n := countChecks(t, id); n != 1 {
		t.Fatalf("%d check rows after the first race, want 1", n)
	}
	if state, _ := readState(t, id); state != domain.StateDown {
		t.Fatalf("state = %s after the first race, want down", state)
	}
	if n := countIncidents(t, id, true); n != 1 {
		t.Fatalf("%d unresolved incidents after the first race, want 1", n)
	}

	// Rounds 1..30: 8 recorders with random outcomes per round.
	const (
		rounds      = 30
		roundRacers = 8
	)
	seed := uint64(time.Now().UnixNano())
	t.Logf("random outcome seed: %d", seed)
	r := rand.New(rand.NewPCG(seed, 0))
	for round := 1; round <= rounds; round++ {
		outcomes := make([]domain.CheckOutcome, roundRacers)
		for i := range outcomes {
			outcomes[i] = randomOutcome(r)
		}
		claimed := armLease(t, id, recordWorker)
		winner := requireOneWinner(t, round, recordConcurrently(stores[:roundRacers], id, claimed, outcomes))

		if n := countChecks(t, id); n != round+1 {
			t.Fatalf("round %d: %d check rows, want %d", round, n, round+1)
		}
		state, _ := readState(t, id)
		if state != winner.change.To {
			t.Fatalf("round %d: stored state %s, but the winner returned %s", round, state, winner.change.To)
		}
		unresolved := countIncidents(t, id, true)
		if unresolved > 1 {
			t.Fatalf("round %d: %d unresolved incidents, never more than 1", round, unresolved)
		}
		if (state == domain.StateDown) != (unresolved == 1) {
			t.Fatalf("round %d: state %s with %d unresolved incidents; want down ⇔ exactly 1", round, state, unresolved)
		}
		if lease := readLease(t, id); lease.owner != nil {
			t.Fatalf("round %d: lease_owner = %v after the round, want released", round, *lease.owner)
		}
	}
}

// TestRecordReadsStateUnderRowLock [DoD 2]: Record waits for a concurrent
// holder of the monitor row lock and computes the transition from the row as
// that holder committed it, not from anything read earlier.
func TestRecordReadsStateUnderRowLock(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]
	claimed := armLease(t, id, recordWorker)

	holder := dedicatedConn(t)
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), "SELECT 1 FROM monitors WHERE id = $1 FOR UPDATE", id); err != nil {
		t.Fatalf("locking the monitor row: %v", err)
	}

	done := make(chan recordResult, 1)
	go func() {
		change, err := store.Record(context.Background(), recordWorker, id, claimed, failureOutcome())
		done <- recordResult{change: change, err: err}
	}()

	select {
	case r := <-done:
		t.Fatalf("Record() finished while the row was locked: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := tx.Exec(t.Context(),
		"UPDATE monitors SET state = 'degraded', consecutive_failures = $2 WHERE id = $1",
		id, seededThreshold-1); err != nil {
		t.Fatalf("updating the locked row: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("committing the lock holder: %v", err)
	}

	var r recordResult
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Record() did not finish after the lock was released")
	}
	if r.err != nil {
		t.Fatalf("Record() returned %v", r.err)
	}
	want := domain.StateChange{
		From:                domain.StateDegraded,
		To:                  domain.StateDown,
		ConsecutiveFailures: seededThreshold,
		Incident:            domain.IncidentOpen,
	}
	if r.change != want {
		t.Fatalf("change = %+v, want %+v (computed from the committed row)", r.change, want)
	}
	if state, failures := readState(t, id); state != domain.StateDown || failures != seededThreshold {
		t.Errorf("stored state = %s with %d failures, want down with %d", state, failures, seededThreshold)
	}
	if n := countIncidents(t, id, true); n != 1 {
		t.Errorf("%d unresolved incidents, want 1", n)
	}
}

// TestRecordRollsBackOnIncidentFailure [DoD 2]: when opening the incident
// fails, nothing of the result persists and the lease stays held.
func TestRecordRollsBackOnIncidentFailure(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]
	if _, err := sharedPool.Exec(t.Context(), `
		UPDATE monitors
		SET state = 'degraded', consecutive_failures = $2,
		    last_checked_at = now() - interval '1 minute',
		    last_status_code = 503, last_latency_ms = 77
		WHERE id = $1`, id, seededThreshold-1); err != nil {
		t.Fatalf("preparing the monitor: %v", err)
	}
	claimed := armLease(t, id, recordWorker)

	// A test-only trigger that makes every incident insert fail. It lives in
	// the shared database, so it is dropped however the test ends.
	if _, err := sharedPool.Exec(t.Context(), `
		CREATE FUNCTION test_reject_incident() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'test: incident insert rejected';
		END
		$$`); err != nil {
		t.Fatalf("creating the trigger function: %v", err)
	}
	t.Cleanup(func() {
		if _, err := sharedPool.Exec(context.Background(), "DROP FUNCTION IF EXISTS test_reject_incident() CASCADE"); err != nil {
			t.Errorf("dropping the test trigger: %v", err)
		}
	})
	if _, err := sharedPool.Exec(t.Context(), `
		CREATE TRIGGER test_reject_incident BEFORE INSERT ON incidents
		FOR EACH ROW EXECUTE FUNCTION test_reject_incident()`); err != nil {
		t.Fatalf("creating the trigger: %v", err)
	}

	type snapshot struct {
		state          string
		failures       int32
		lastCheckedAt  *time.Time
		lastStatusCode *int32
		lastLatencyMS  *int32
		leaseOwner     *string
		leaseUntil     *time.Time
		nextCheckAt    time.Time
	}
	read := func() snapshot {
		t.Helper()
		var s snapshot
		if err := sharedPool.QueryRow(t.Context(), `
			SELECT state, consecutive_failures, last_checked_at, last_status_code,
			       last_latency_ms, lease_owner, lease_until, next_check_at
			FROM monitors WHERE id = $1`, id).
			Scan(&s.state, &s.failures, &s.lastCheckedAt, &s.lastStatusCode,
				&s.lastLatencyMS, &s.leaseOwner, &s.leaseUntil, &s.nextCheckAt); err != nil {
			t.Fatalf("reading the monitor: %v", err)
		}
		return s
	}
	before := read()

	_, err := store.Record(t.Context(), recordWorker, id, claimed, failureOutcome())
	if err == nil || errors.Is(err, scheduler.ErrLeaseLost) {
		t.Fatalf("Record() returned %v, want an error that is not ErrLeaseLost", err)
	}

	if n := countChecks(t, id); n != 0 {
		t.Errorf("%d check rows after the rollback, want 0", n)
	}
	if n := countIncidents(t, id, false); n != 0 {
		t.Errorf("%d incidents after the rollback, want 0", n)
	}
	after := read()
	if after.state != before.state || after.failures != before.failures ||
		!equalTime(after.lastCheckedAt, before.lastCheckedAt) ||
		!equalPtr(after.lastStatusCode, before.lastStatusCode) ||
		!equalPtr(after.lastLatencyMS, before.lastLatencyMS) ||
		!after.nextCheckAt.Equal(before.nextCheckAt) {
		t.Errorf("monitor changed by a rolled-back Record:\n before %+v\n after  %+v", before, after)
	}
	if after.leaseOwner == nil || *after.leaseOwner != recordWorker ||
		!equalTime(after.leaseUntil, before.leaseUntil) {
		t.Errorf("lease = (%v, %v), want still held by %s until %v",
			deref(after.leaseOwner), deref(after.leaseUntil), recordWorker, deref(before.leaseUntil))
	}
}

// TestRecordWithIncidentAlreadyOpen: a hand-edited monitor that reaches down
// while an incident is already open still records the result, keeps the one
// incident, and reports IncidentAlreadyOpen so the scheduler can warn.
func TestRecordWithIncidentAlreadyOpen(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]
	setState(t, id, domain.StateDegraded, seededThreshold-1)
	if err := insertIncident(t, id, time.Now().Add(-time.Hour), nil); err != nil {
		t.Fatalf("inserting the open incident: %v", err)
	}
	claimed := armLease(t, id, recordWorker)

	change, err := store.Record(t.Context(), recordWorker, id, claimed, failureOutcome())
	if err != nil {
		t.Fatalf("Record() returned %v", err)
	}
	if change.Incident != domain.IncidentOpen || !change.IncidentAlreadyOpen {
		t.Errorf("change = %+v, want IncidentOpen with IncidentAlreadyOpen", change)
	}
	if state, _ := readState(t, id); state != domain.StateDown {
		t.Errorf("state = %s, want down", state)
	}
	if n := countChecks(t, id); n != 1 {
		t.Errorf("%d check rows, want 1", n)
	}
	if n := countIncidents(t, id, false); n != 1 {
		t.Errorf("%d incidents, want the 1 that was already open", n)
	}
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
