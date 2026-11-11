//go:build integration

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/scheduler"
)

// testLease is the lease the tests ask for. Monitors are seeded with a 5s
// timeout, so the per-row floor (timeout + 5s = 10s) is below it and the
// requested lease is the one that applies.
const testLease = 30 * time.Second

// newSchedulerStore returns a scheduler store over empty tables.
func newSchedulerStore(t *testing.T) *repository.SchedulerStore {
	t.Helper()

	truncateMonitors(t)
	return repository.NewSchedulerStore(sharedPool)
}

// seedSpec describes monitors to insert directly with SQL, bypassing the
// monitor service so tests can create states the API never would (already
// due, deleted, far in the future).
type seedSpec struct {
	count     int
	enabled   bool
	deleted   bool
	timeoutMS int
	// dueIn is added to now() for next_check_at; negative means already due.
	dueIn time.Duration
}

func dueSpec(count int) seedSpec {
	return seedSpec{count: count, enabled: true, timeoutMS: 5000, dueIn: -time.Minute}
}

// seedMonitors inserts spec.count monitors in one statement and returns their
// ids.
func seedMonitors(t *testing.T, spec seedSpec) []uuid.UUID {
	t.Helper()

	var deletedAt *time.Time
	if spec.deleted {
		deletedAt = ptr(time.Now())
	}

	rows, err := sharedPool.Query(t.Context(), `
		INSERT INTO monitors (name, url, interval_seconds, timeout_ms,
		                      expected_status_min, expected_status_max, failure_threshold,
		                      enabled, next_check_at, deleted_at)
		SELECT 'seed-' || g, 'https://example.com/' || g, 60, $2,
		       200, 399, 3, $3, now() + make_interval(secs => $4::float8), $5
		FROM generate_series(1, $1) AS g
		RETURNING id`,
		spec.count, spec.timeoutMS, spec.enabled, spec.dueIn.Seconds(), deletedAt)
	if err != nil {
		t.Fatalf("seeding monitors failed: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatalf("collecting seeded ids failed: %v", err)
	}
	if len(ids) != spec.count {
		t.Fatalf("seeded %d monitors, want %d", len(ids), spec.count)
	}
	return ids
}

// dedicatedConn opens a connection of its own, outside the shared pool, so a
// test can change session settings or hold locks without leaking either into
// other tests.
func dedicatedConn(t *testing.T) *pgx.Conn {
	t.Helper()

	conn, err := pgx.ConnectConfig(t.Context(), sharedPool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("opening a dedicated connection failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

type leaseRow struct {
	owner       *string
	until       *time.Time
	nextCheckAt time.Time
}

func readLease(t *testing.T, id uuid.UUID) leaseRow {
	t.Helper()

	var r leaseRow
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT lease_owner, lease_until, next_check_at FROM monitors WHERE id = $1", id).
		Scan(&r.owner, &r.until, &r.nextCheckAt); err != nil {
		t.Fatalf("reading the lease of %s failed: %v", id, err)
	}
	return r
}

func dbNow(t *testing.T) time.Time {
	t.Helper()

	var now time.Time
	if err := sharedPool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("reading now() failed: %v", err)
	}
	return now
}

func countChecks(t *testing.T, id uuid.UUID) int {
	t.Helper()

	var n int
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT count(*) FROM monitor_checks WHERE monitor_id = $1", id).Scan(&n); err != nil {
		t.Fatalf("counting checks failed: %v", err)
	}
	return n
}

func ids(monitors []domain.Monitor) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(monitors))
	for _, m := range monitors {
		out = append(out, m.ID)
	}
	return out
}

func successOutcome() domain.CheckOutcome {
	return domain.CheckOutcome{Success: true, StatusCode: ptr(int32(200)), LatencyMS: 42}
}

// TestClaimParallelClaimersNeverDoubleAssign is the core DoD proof: claimers
// racing on the same due set, each on its own connection, never receive the
// same monitor twice in a round, and together they claim every monitor.
func TestClaimParallelClaimersNeverDoubleAssign(t *testing.T) {
	const (
		monitorCount = 300
		claimerCount = 8
		batchSize    = 7
		rounds       = 5
	)

	newSchedulerStore(t)
	seeded := seedMonitors(t, dueSpec(monitorCount))

	stores := make([]*repository.SchedulerStore, claimerCount)
	for i := range stores {
		stores[i] = repository.NewSchedulerStore(dedicatedConn(t))
	}

	for round := range rounds {
		if round > 0 {
			// Re-arm: every monitor due again with no lease.
			if _, err := sharedPool.Exec(t.Context(), `
				UPDATE monitors
				SET lease_owner = NULL, lease_until = NULL, next_check_at = now() - interval '1 minute'`); err != nil {
				t.Fatalf("round %d: re-arming monitors failed: %v", round, err)
			}
		}

		claimed := make([][]uuid.UUID, claimerCount)
		errs := make([]error, claimerCount)
		start := make(chan struct{})
		var ready, done sync.WaitGroup

		for i := range claimerCount {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				workerID := fmt.Sprintf("worker-%d", i)
				ready.Done()
				<-start
				for {
					batch, err := stores[i].Claim(t.Context(), workerID, batchSize, testLease)
					if err != nil {
						errs[i] = err
						return
					}
					if len(batch) == 0 {
						return
					}
					if len(batch) > batchSize {
						errs[i] = fmt.Errorf("claim returned %d monitors, batch size is %d", len(batch), batchSize)
						return
					}
					claimed[i] = append(claimed[i], ids(batch)...)
				}
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: claimer %d failed: %v", round, i, err)
			}
		}

		owners := make(map[uuid.UUID]string, monitorCount)
		active := 0
		for i, list := range claimed {
			if len(list) > 0 {
				active++
			}
			workerID := fmt.Sprintf("worker-%d", i)
			for _, id := range list {
				if prev, dup := owners[id]; dup {
					t.Errorf("round %d: monitor %s claimed by both %s and %s", round, id, prev, workerID)
				}
				owners[id] = workerID
			}
		}
		if len(owners) != monitorCount {
			t.Errorf("round %d: %d distinct monitors claimed, want all %d", round, len(owners), monitorCount)
		}
		for _, id := range seeded {
			if _, ok := owners[id]; !ok {
				t.Errorf("round %d: monitor %s was never claimed", round, id)
			}
		}

		rows, err := sharedPool.Query(t.Context(), "SELECT id, lease_owner FROM monitors")
		if err != nil {
			t.Fatalf("round %d: reading lease owners failed: %v", round, err)
		}
		type ownerRow struct {
			ID    uuid.UUID
			Owner *string
		}
		dbOwners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ownerRow])
		if err != nil {
			t.Fatalf("round %d: collecting lease owners failed: %v", round, err)
		}
		for _, r := range dbOwners {
			if r.Owner == nil || *r.Owner != owners[r.ID] {
				t.Errorf("round %d: monitor %s has lease_owner %v in the database, want %q",
					round, r.ID, r.Owner, owners[r.ID])
			}
		}
		t.Logf("round %d: %d monitors claimed by %d of %d claimers", round, len(owners), active, claimerCount)
	}
}

// TestClaimSkipsRowsLockedByOpenTransaction proves the claim uses SKIP LOCKED
// specifically: plain FOR UPDATE would block on A's locks until B's
// statement_timeout fired.
func TestClaimSkipsRowsLockedByOpenTransaction(t *testing.T) {
	newSchedulerStore(t)
	seedMonitors(t, dueSpec(10))

	connA := dedicatedConn(t)
	txA, err := connA.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin transaction A: %v", err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()

	claimedA, err := repository.NewSchedulerStore(txA).Claim(t.Context(), "worker-a", 5, testLease)
	if err != nil {
		t.Fatalf("A: Claim() returned an unexpected error: %v", err)
	}
	if len(claimedA) != 5 {
		t.Fatalf("A claimed %d monitors, want 5", len(claimedA))
	}

	connB := dedicatedConn(t)
	if _, err := connB.Exec(t.Context(), "SET statement_timeout = '2s'"); err != nil {
		t.Fatalf("setting statement_timeout on B: %v", err)
	}

	started := time.Now()
	claimedB, err := repository.NewSchedulerStore(connB).Claim(t.Context(), "worker-b", 10, testLease)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("B: Claim() returned %v after %s, want it to skip A's locked rows", err, elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("B's claim took %s, want it to return immediately rather than wait on locks", elapsed)
	}
	if len(claimedB) == 0 {
		t.Fatal("B claimed nothing, want the 5 monitors A did not lock")
	}

	lockedByA := make(map[uuid.UUID]bool, len(claimedA))
	for _, m := range claimedA {
		lockedByA[m.ID] = true
	}
	for _, m := range claimedB {
		if lockedByA[m.ID] {
			t.Errorf("B claimed %s, which A holds locked in its open transaction", m.ID)
		}
	}
	if len(claimedB) != 5 {
		t.Errorf("B claimed %d monitors, want the 5 A did not lock", len(claimedB))
	}
}

// TestExpiredLeaseReclaimedByDifferentWorker covers a worker that stalls past
// its lease: another worker takes the monitor over, and the stale worker's
// late result is rejected rather than written as a duplicate.
func TestExpiredLeaseReclaimedByDifferentWorker(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	claimedA, err := store.Claim(t.Context(), "worker-a", 10, testLease)
	if err != nil || len(claimedA) != 1 {
		t.Fatalf("A: Claim() = %d monitors, %v; want 1, nil", len(claimedA), err)
	}

	if _, err := sharedPool.Exec(t.Context(),
		"UPDATE monitors SET lease_until = now() - interval '1 second' WHERE id = $1", id); err != nil {
		t.Fatalf("expiring A's lease failed: %v", err)
	}

	claimedB, err := store.Claim(t.Context(), "worker-b", 10, testLease)
	if err != nil {
		t.Fatalf("B: Claim() returned an unexpected error: %v", err)
	}
	if len(claimedB) != 1 || claimedB[0].ID != id {
		t.Fatalf("B claimed %v, want the monitor whose lease expired", ids(claimedB))
	}
	if got := readLease(t, id); got.owner == nil || *got.owner != "worker-b" {
		t.Fatalf("lease_owner = %v after reclaim, want worker-b", got.owner)
	}

	_, err = store.Record(t.Context(), "worker-a", id, claimedA[0].NextCheckAt, successOutcome())
	if !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("A's late Record() returned %v, want scheduler.ErrLeaseLost", err)
	}
	if n := countChecks(t, id); n != 0 {
		t.Errorf("%d monitor_checks rows after a lost-lease Record, want 0", n)
	}
	if got := readLease(t, id); got.owner == nil || *got.owner != "worker-b" {
		t.Errorf("lease_owner = %v after A's late Record, want B's lease untouched", got.owner)
	}

	if _, err := store.Record(t.Context(), "worker-b", id, claimedB[0].NextCheckAt, successOutcome()); err != nil {
		t.Fatalf("B: Record() returned an unexpected error: %v", err)
	}
	if n := countChecks(t, id); n != 1 {
		t.Errorf("%d monitor_checks rows after B's Record, want exactly 1", n)
	}
}

func TestClaimIgnoresActiveLease(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	if claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease); err != nil || len(claimed) != 1 {
		t.Fatalf("A: Claim() = %d monitors, %v; want 1, nil", len(claimed), err)
	}
	before := readLease(t, id)

	claimed, err := store.Claim(t.Context(), "worker-b", 10, testLease)
	if err != nil {
		t.Fatalf("B: Claim() returned an unexpected error: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("B claimed %v, want nothing while A's lease is active", ids(claimed))
	}

	after := readLease(t, id)
	if after.owner == nil || *after.owner != "worker-a" || !after.until.Equal(*before.until) {
		t.Errorf("lease = (%v, %v), want A's lease (worker-a, %v) unchanged", after.owner, after.until, before.until)
	}
}

func TestClaimSkipsDisabledDeletedAndNotDue(t *testing.T) {
	store := newSchedulerStore(t)

	due := seedMonitors(t, dueSpec(1))[0]

	disabled := dueSpec(1)
	disabled.enabled = false
	seedMonitors(t, disabled)

	deleted := dueSpec(1)
	deleted.deleted = true
	seedMonitors(t, deleted)

	notDue := dueSpec(1)
	notDue.dueIn = time.Hour
	seedMonitors(t, notDue)

	claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease)
	if err != nil {
		t.Fatalf("Claim() returned an unexpected error: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != due {
		t.Errorf("Claim() returned %v, want only the enabled, live, due monitor %s", ids(claimed), due)
	}

	var leased int
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT count(*) FROM monitors WHERE lease_owner IS NOT NULL").Scan(&leased); err != nil {
		t.Fatalf("counting leases failed: %v", err)
	}
	if leased != 1 {
		t.Errorf("%d monitors hold a lease, want 1", leased)
	}
}

func TestClaimRespectsBatchSize(t *testing.T) {
	store := newSchedulerStore(t)
	seedMonitors(t, dueSpec(10))

	seen := make(map[uuid.UUID]bool)
	for i, want := range []int{3, 3, 3, 1, 0} {
		claimed, err := store.Claim(t.Context(), "worker-a", 3, testLease)
		if err != nil {
			t.Fatalf("claim %d: Claim() returned an unexpected error: %v", i, err)
		}
		if len(claimed) != want {
			t.Errorf("claim %d returned %d monitors, want %d", i, len(claimed), want)
		}
		for _, m := range claimed {
			if seen[m.ID] {
				t.Errorf("claim %d returned %s again", i, m.ID)
			}
			seen[m.ID] = true
		}
	}
}

// TestClaimLeaseCoversMonitorTimeout checks the per-row lease floor: a
// monitor's own timeout plus a margin wins over a shorter configured lease,
// so a slow check cannot outlive its lease.
func TestClaimLeaseCoversMonitorTimeout(t *testing.T) {
	store := newSchedulerStore(t)

	slow := dueSpec(1)
	slow.timeoutMS = 60000
	slowID := seedMonitors(t, slow)[0]

	fast := dueSpec(1)
	fast.timeoutMS = 1000
	fastID := seedMonitors(t, fast)[0]

	const lease = 10 * time.Second
	before := dbNow(t)
	claimed, err := store.Claim(t.Context(), "worker-a", 10, lease)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("Claim() = %d monitors, %v; want 2, nil", len(claimed), err)
	}
	after := dbNow(t)

	tests := []struct {
		name string
		id   uuid.UUID
		want time.Duration
	}{
		{name: "60s timeout outlasts the lease", id: slowID, want: 65 * time.Second},
		{name: "1s timeout gets the configured lease", id: fastID, want: lease},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readLease(t, tt.id)
			if got.until == nil {
				t.Fatal("lease_until is NULL after claim")
			}
			if got.until.Before(before.Add(tt.want)) || got.until.After(after.Add(tt.want)) {
				t.Errorf("lease_until = %v, want now + %s, between %v and %v",
					got.until, tt.want, before.Add(tt.want), after.Add(tt.want))
			}
		})
	}
}

func TestRecordCheckResult(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	tests := []struct {
		name    string
		outcome domain.CheckOutcome
	}{
		{name: "success", outcome: successOutcome()},
		{name: "failure", outcome: domain.CheckOutcome{
			Success:      false,
			LatencyMS:    7,
			ErrorKind:    ptr(domain.ErrorKindConnection),
			ErrorMessage: ptr("connection refused"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := sharedPool.Exec(t.Context(),
				"UPDATE monitors SET next_check_at = now() - interval '1 minute' WHERE id = $1", id); err != nil {
				t.Fatalf("making the monitor due failed: %v", err)
			}
			claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("Claim() = %d monitors, %v; want 1, nil", len(claimed), err)
			}

			before := dbNow(t)
			if _, err := store.Record(t.Context(), "worker-a", id, claimed[0].NextCheckAt, tt.outcome); err != nil {
				t.Fatalf("Record() returned an unexpected error: %v", err)
			}
			after := dbNow(t)

			var (
				success      bool
				statusCode   *int32
				latencyMS    int32
				errorKind    *string
				errorMessage *string
				checkedAt    time.Time
			)
			if err := sharedPool.QueryRow(t.Context(), `
				SELECT success, status_code, latency_ms, error_kind, error_message, checked_at
				FROM monitor_checks WHERE monitor_id = $1
				ORDER BY id DESC LIMIT 1`, id).
				Scan(&success, &statusCode, &latencyMS, &errorKind, &errorMessage, &checkedAt); err != nil {
				t.Fatalf("reading the recorded check failed: %v", err)
			}
			o := tt.outcome
			if success != o.Success || latencyMS != o.LatencyMS ||
				!equalPtr(statusCode, o.StatusCode) || !equalPtr(errorMessage, o.ErrorMessage) {
				t.Errorf("stored check = (success %v, status %v, latency %d, message %v), want %+v",
					success, deref(statusCode), latencyMS, deref(errorMessage), o)
			}
			wantKind := (*string)(nil)
			if o.ErrorKind != nil {
				wantKind = ptr(o.ErrorKind.String())
			}
			if !equalPtr(errorKind, wantKind) {
				t.Errorf("error_kind = %v, want %v", deref(errorKind), deref(wantKind))
			}
			if checkedAt.Before(before) || checkedAt.After(after) {
				t.Errorf("checked_at = %v, want PostgreSQL's now() at record time", checkedAt)
			}

			var (
				lastCheckedAt  *time.Time
				lastStatusCode *int32
				lastLatencyMS  *int32
			)
			if err := sharedPool.QueryRow(t.Context(),
				"SELECT last_checked_at, last_status_code, last_latency_ms FROM monitors WHERE id = $1", id).
				Scan(&lastCheckedAt, &lastStatusCode, &lastLatencyMS); err != nil {
				t.Fatalf("reading last_* fields failed: %v", err)
			}
			if lastCheckedAt == nil || !lastCheckedAt.Equal(checkedAt) {
				t.Errorf("last_checked_at = %v, want %v", lastCheckedAt, checkedAt)
			}
			if !equalPtr(lastStatusCode, o.StatusCode) || lastLatencyMS == nil || *lastLatencyMS != o.LatencyMS {
				t.Errorf("last_status_code, last_latency_ms = %v, %v; want %v, %d",
					deref(lastStatusCode), deref(lastLatencyMS), deref(o.StatusCode), o.LatencyMS)
			}

			lease := readLease(t, id)
			if lease.owner != nil || lease.until != nil {
				t.Errorf("lease = (%v, %v) after Record, want both cleared", lease.owner, lease.until)
			}
			interval := 60 * time.Second
			if lease.nextCheckAt.Before(before.Add(interval)) || lease.nextCheckAt.After(after.Add(interval)) {
				t.Errorf("next_check_at = %v, want now + %s (between %v and %v)",
					lease.nextCheckAt, interval, before.Add(interval), after.Add(interval))
			}
		})
	}

	if n := countChecks(t, id); n != len(tests) {
		t.Errorf("%d monitor_checks rows, want %d", n, len(tests))
	}
}

// TestRecordPreservesManualRequestDuringCheck covers a manual check request
// that lands while the scheduled check is running: the result must not
// overwrite the request with the routine next slot.
func TestRecordPreservesManualRequestDuringCheck(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %d monitors, %v; want 1, nil", len(claimed), err)
	}

	// What POST /v1/monitors/{id}/checks runs (RequestCheck).
	var requested time.Time
	if err := sharedPool.QueryRow(t.Context(),
		"UPDATE monitors SET next_check_at = now() WHERE id = $1 RETURNING next_check_at", id).
		Scan(&requested); err != nil {
		t.Fatalf("requesting a manual check failed: %v", err)
	}
	if requested.Equal(claimed[0].NextCheckAt) {
		t.Fatal("the manual request did not change next_check_at; the test proves nothing")
	}

	if _, err := store.Record(t.Context(), "worker-a", id, claimed[0].NextCheckAt, successOutcome()); err != nil {
		t.Fatalf("Record() returned an unexpected error: %v", err)
	}

	got := readLease(t, id)
	if !got.nextCheckAt.Equal(requested) {
		t.Errorf("next_check_at = %v after Record, want the manual request %v kept", got.nextCheckAt, requested)
	}
	if got.owner != nil || got.until != nil {
		t.Errorf("lease = (%v, %v) after Record, want both cleared", got.owner, got.until)
	}
	if n := countChecks(t, id); n != 1 {
		t.Errorf("%d monitor_checks rows, want 1", n)
	}
}

// TestRecordOnDeletedMonitorLosesLease: soft delete clears the lease, so a
// result for a monitor deleted mid-check is discarded.
func TestRecordOnDeletedMonitorLosesLease(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %d monitors, %v; want 1, nil", len(claimed), err)
	}
	if err := repository.NewMonitorStore(sharedPool).SoftDelete(t.Context(), id); err != nil {
		t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
	}

	_, err = store.Record(t.Context(), "worker-a", id, claimed[0].NextCheckAt, successOutcome())
	if !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("Record() on a deleted monitor returned %v, want scheduler.ErrLeaseLost", err)
	}
	if n := countChecks(t, id); n != 0 {
		t.Errorf("%d monitor_checks rows for a deleted monitor, want 0", n)
	}
}

func TestReleaseLease(t *testing.T) {
	store := newSchedulerStore(t)
	id := seedMonitors(t, dueSpec(1))[0]

	claimed, err := store.Claim(t.Context(), "worker-a", 10, testLease)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %d monitors, %v; want 1, nil", len(claimed), err)
	}

	if err := store.Release(t.Context(), "worker-b", id); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("Release() by a non-owner returned %v, want scheduler.ErrLeaseLost", err)
	}
	if got := readLease(t, id); got.owner == nil || *got.owner != "worker-a" {
		t.Fatalf("lease_owner = %v after a non-owner release, want worker-a", got.owner)
	}

	if err := store.Release(t.Context(), "worker-a", id); err != nil {
		t.Fatalf("Release() by the owner returned an unexpected error: %v", err)
	}
	got := readLease(t, id)
	if got.owner != nil || got.until != nil {
		t.Errorf("lease = (%v, %v) after release, want both cleared", got.owner, got.until)
	}
	if !got.nextCheckAt.Equal(claimed[0].NextCheckAt) {
		t.Errorf("next_check_at = %v after release, want it unchanged at %v", got.nextCheckAt, claimed[0].NextCheckAt)
	}

	if err := store.Release(t.Context(), "worker-a", id); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("a second Release() returned %v, want scheduler.ErrLeaseLost", err)
	}
	if _, err := store.Record(t.Context(), "worker-a", id, claimed[0].NextCheckAt, successOutcome()); !errors.Is(err, scheduler.ErrLeaseLost) {
		t.Errorf("Record() after release returned %v, want scheduler.ErrLeaseLost", err)
	}

	reclaimed, err := store.Claim(t.Context(), "worker-b", 10, testLease)
	if err != nil {
		t.Fatalf("Claim() after release returned an unexpected error: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != id {
		t.Errorf("Claim() after release returned %v, want the released monitor, still due", ids(reclaimed))
	}
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
