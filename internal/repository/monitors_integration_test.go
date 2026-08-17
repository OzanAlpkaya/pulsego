//go:build integration

package repository_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
)

// newStore returns a store over an empty monitors table. Tests sharing the
// database run serially, so none of them call t.Parallel.
func newStore(t *testing.T) *repository.MonitorStore {
	t.Helper()

	truncateMonitors(t)
	return repository.NewMonitorStore(sharedPool)
}

func validCreate() monitor.CreateParams {
	return monitor.CreateParams{
		Name:              "Example",
		URL:               "https://example.com",
		IntervalSeconds:   60,
		TimeoutMS:         5000,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 399,
		FailureThreshold:  3,
		Enabled:           true,
	}
}

func ptr[T any](v T) *T { return &v }

func TestCreateAndGet(t *testing.T) {
	store := newStore(t)

	created, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	if created.ID == uuid.Nil {
		t.Error("Create() returned the zero UUID, want a generated id")
	}
	if created.State != domain.StateUnknown {
		t.Errorf("State = %q, want %q for a monitor that has never been checked",
			created.State, domain.StateUnknown)
	}
	if created.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", created.ConsecutiveFailures)
	}
	if created.LastCheckedAt != nil {
		t.Error("LastCheckedAt is set on a new monitor, want nil")
	}
	if created.LeaseOwner != nil || created.LeaseUntil != nil {
		t.Error("a new monitor holds a lease, want none")
	}
	if created.NextCheckAt.IsZero() {
		t.Error("NextCheckAt is zero, want the monitor to be due immediately")
	}

	got, err := store.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	if got.ID != created.ID || got.Name != "Example" || got.URL != "https://example.com" {
		t.Errorf("Get() returned %+v, want the created monitor", got)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	store := newStore(t)

	_, err := store.Get(t.Context(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() returned %v, want domain.ErrNotFound", err)
	}
}

// TestConstraintViolationBecomesValidationError covers the safety net: if
// application validation were ever bypassed, the API must still answer 422
// rather than 500.
func TestConstraintViolationBecomesValidationError(t *testing.T) {
	store := newStore(t)

	tests := []struct {
		name      string
		mutate    func(*monitor.CreateParams)
		wantField string
	}{
		{name: "interval", mutate: func(p *monitor.CreateParams) { p.IntervalSeconds = 1 }, wantField: "interval_seconds"},
		{name: "timeout", mutate: func(p *monitor.CreateParams) { p.TimeoutMS = 1 }, wantField: "timeout_ms"},
		{name: "threshold", mutate: func(p *monitor.CreateParams) { p.FailureThreshold = 99 }, wantField: "failure_threshold"},
		{name: "blank name", mutate: func(p *monitor.CreateParams) { p.Name = "  " }, wantField: "name"},
		{name: "status order", mutate: func(p *monitor.CreateParams) { p.ExpectedStatusMin, p.ExpectedStatusMax = 400, 200 }, wantField: "expected_status_min"},
		{
			name:      "url length",
			mutate:    func(p *monitor.CreateParams) { p.URL = "https://example.com/" + strings.Repeat("a", 2100) },
			wantField: "url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := validCreate()
			tt.mutate(&params)

			_, err := store.Create(t.Context(), params)

			var verr *monitor.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Create() returned %v (%T), want a *monitor.ValidationError", err, err)
			}
			if len(verr.Fields) == 0 || verr.Fields[0].Field != tt.wantField {
				t.Errorf("rejected fields = %+v, want %q", verr.Fields, tt.wantField)
			}
		})
	}
}

func TestUpdatePartial(t *testing.T) {
	store := newStore(t)

	created, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	// Postgres now() is transaction-scoped, so a distinct updated_at requires
	// a separate statement rather than merely a later wall-clock read.
	updated, err := store.Update(t.Context(), monitor.UpdateParams{
		ID:      created.ID,
		Name:    ptr("Renamed"),
		Enabled: ptr(false),
	})
	if err != nil {
		t.Fatalf("Update() returned an unexpected error: %v", err)
	}

	if updated.Name != "Renamed" {
		t.Errorf("Name = %q, want %q", updated.Name, "Renamed")
	}
	if updated.Enabled {
		t.Error("Enabled = true, want the update to have disabled the monitor")
	}
	if updated.URL != created.URL {
		t.Errorf("URL = %q, want the untouched %q", updated.URL, created.URL)
	}
	if updated.IntervalSeconds != created.IntervalSeconds {
		t.Errorf("IntervalSeconds = %d, want the untouched %d", updated.IntervalSeconds, created.IntervalSeconds)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want it to advance past %v", updated.UpdatedAt, created.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want it unchanged at %v", updated.CreatedAt, created.CreatedAt)
	}
}

// TestUpdateShorteningIntervalPullsNextCheckForward covers decision D3: a
// tightened schedule has to take effect without waiting out the old interval.
func TestUpdateShorteningIntervalPullsNextCheckForward(t *testing.T) {
	store := newStore(t)

	params := validCreate()
	params.IntervalSeconds = 86400
	created, err := store.Create(t.Context(), params)
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	// Push the next check a day out, as a completed check would.
	far := time.Now().Add(24 * time.Hour)
	if _, err := sharedPool.Exec(t.Context(),
		"UPDATE monitors SET next_check_at = $1 WHERE id = $2", far, created.ID); err != nil {
		t.Fatalf("seeding next_check_at failed: %v", err)
	}

	updated, err := store.Update(t.Context(), monitor.UpdateParams{
		ID:              created.ID,
		IntervalSeconds: ptr(int32(60)),
	})
	if err != nil {
		t.Fatalf("Update() returned an unexpected error: %v", err)
	}

	if !updated.NextCheckAt.Before(far) {
		t.Errorf("NextCheckAt = %v, want it pulled in from %v after shortening the interval",
			updated.NextCheckAt, far)
	}
	if limit := time.Now().Add(61 * time.Second); updated.NextCheckAt.After(limit) {
		t.Errorf("NextCheckAt = %v, want it within the new 60s interval (before %v)",
			updated.NextCheckAt, limit)
	}
}

// TestUpdateDoesNotPushNextCheckOut is the other half of D3: editing an
// unrelated field must not delay a check that is already due.
func TestUpdateDoesNotPushNextCheckOut(t *testing.T) {
	store := newStore(t)

	created, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	updated, err := store.Update(t.Context(), monitor.UpdateParams{
		ID:   created.ID,
		Name: ptr("Renamed"),
	})
	if err != nil {
		t.Fatalf("Update() returned an unexpected error: %v", err)
	}

	if !updated.NextCheckAt.Equal(created.NextCheckAt) {
		t.Errorf("NextCheckAt moved from %v to %v on an unrelated edit",
			created.NextCheckAt, updated.NextCheckAt)
	}
}

func TestUpdateMissingReturnsNotFound(t *testing.T) {
	store := newStore(t)

	_, err := store.Update(t.Context(), monitor.UpdateParams{ID: uuid.New(), Name: ptr("x")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Update() returned %v, want domain.ErrNotFound", err)
	}
}

func TestSoftDelete(t *testing.T) {
	store := newStore(t)

	created, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	if err := store.SoftDelete(t.Context(), created.ID); err != nil {
		t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
	}

	// The row must survive, so that historical checks stay meaningful.
	var deletedAt *time.Time
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT deleted_at FROM monitors WHERE id = $1", created.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("the row was hard-deleted: %v", err)
	}
	if deletedAt == nil {
		t.Error("deleted_at is NULL after SoftDelete")
	}

	if _, err := store.Get(t.Context(), created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get() after delete returned %v, want domain.ErrNotFound", err)
	}

	page, err := store.List(t.Context(), monitor.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List() returned an unexpected error: %v", err)
	}
	if len(page) != 0 {
		t.Errorf("List() returned %d monitors, want the soft-deleted one excluded", len(page))
	}

	if err := store.SoftDelete(t.Context(), created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deleting twice returned %v, want domain.ErrNotFound", err)
	}
	if _, err := store.Update(t.Context(), monitor.UpdateParams{ID: created.ID, Name: ptr("x")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("updating a deleted monitor returned %v, want domain.ErrNotFound", err)
	}
}

// TestSoftDeleteClearsLease stops a worker mid-cycle from continuing to treat a
// deleted monitor as its own.
func TestSoftDeleteClearsLease(t *testing.T) {
	store := newStore(t)

	created, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	if _, err := sharedPool.Exec(t.Context(),
		"UPDATE monitors SET lease_owner = $1, lease_until = $2 WHERE id = $3",
		"worker-1", time.Now().Add(time.Minute), created.ID); err != nil {
		t.Fatalf("seeding a lease failed: %v", err)
	}

	if err := store.SoftDelete(t.Context(), created.ID); err != nil {
		t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
	}

	var owner *string
	var until *time.Time
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT lease_owner, lease_until FROM monitors WHERE id = $1", created.ID).Scan(&owner, &until); err != nil {
		t.Fatalf("reading the lease failed: %v", err)
	}
	if owner != nil || until != nil {
		t.Errorf("lease = (%v, %v) after delete, want both cleared", owner, until)
	}
}

// TestListKeysetPagination walks every page of a seeded set and asserts that
// each monitor is seen exactly once. Rows deliberately share a created_at,
// which is the case a naive cursor on the timestamp alone would get wrong.
func TestListKeysetPagination(t *testing.T) {
	store := newStore(t)

	const total = 25
	created := make(map[uuid.UUID]bool, total)
	for i := range total {
		params := validCreate()
		params.Name = "Monitor"
		m, err := store.Create(t.Context(), params)
		if err != nil {
			t.Fatalf("seeding monitor %d failed: %v", i, err)
		}
		created[m.ID] = false
	}

	// Force a shared timestamp so ordering has to fall back to the id.
	shared := time.Now().Add(-time.Hour)
	if _, err := sharedPool.Exec(t.Context(), "UPDATE monitors SET created_at = $1", shared); err != nil {
		t.Fatalf("forcing a shared created_at failed: %v", err)
	}

	const pageSize = 7
	var (
		cursor *monitor.Cursor
		pages  int
		seen   int
	)
	for {
		page, err := store.List(t.Context(), monitor.ListParams{Limit: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("List() returned an unexpected error: %v", err)
		}
		if len(page) == 0 {
			break
		}

		for _, m := range page {
			already, known := created[m.ID]
			if !known {
				t.Fatalf("List() returned an unknown monitor %v", m.ID)
			}
			if already {
				t.Errorf("monitor %v was returned on more than one page", m.ID)
			}
			created[m.ID] = true
			seen++
		}

		last := page[len(page)-1]
		cursor = &monitor.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}

		pages++
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}

	if seen != total {
		t.Errorf("saw %d monitors across %d pages, want %d", seen, pages, total)
	}
	for id, wasSeen := range created {
		if !wasSeen {
			t.Errorf("monitor %v was never returned", id)
		}
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	store := newStore(t)

	for i := range 5 {
		params := validCreate()
		params.Name = "Monitor"
		if _, err := store.Create(t.Context(), params); err != nil {
			t.Fatalf("seeding monitor %d failed: %v", i, err)
		}
	}

	page, err := store.List(t.Context(), monitor.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List() returned an unexpected error: %v", err)
	}
	if len(page) != 5 {
		t.Fatalf("List() returned %d monitors, want 5", len(page))
	}

	for i := 1; i < len(page); i++ {
		prev, cur := page[i-1], page[i]
		if cur.CreatedAt.After(prev.CreatedAt) {
			t.Errorf("row %d (%v) is newer than row %d (%v), want newest first",
				i, cur.CreatedAt, i-1, prev.CreatedAt)
		}
	}
}

func TestListEmptyReturnsEmptySlice(t *testing.T) {
	store := newStore(t)

	page, err := store.List(t.Context(), monitor.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("List() returned an unexpected error: %v", err)
	}
	if page == nil {
		t.Error("List() returned nil, want an empty slice so the API renders []")
	}
	if len(page) != 0 {
		t.Errorf("List() returned %d monitors, want 0", len(page))
	}
}
