package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

func TestRequestCheck(t *testing.T) {
	t.Parallel()

	t.Run("enabled monitor is made due", func(t *testing.T) {
		t.Parallel()

		id := uuid.New()
		store := newFakeStore()
		store.monitors[id] = domain.Monitor{ID: id, Enabled: true}
		svc := monitor.NewService(store, testDefaults())

		got, err := svc.RequestCheck(context.Background(), id)
		if err != nil {
			t.Fatalf("RequestCheck() returned an unexpected error: %v", err)
		}
		if got.ID != id {
			t.Errorf("returned monitor %v, want %v", got.ID, id)
		}
		if store.requestedID == nil || *store.requestedID != id {
			t.Errorf("store received %v, want %v", store.requestedID, id)
		}
	})

	t.Run("missing monitor is not found", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		svc := monitor.NewService(store, testDefaults())

		_, err := svc.RequestCheck(context.Background(), uuid.New())
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("error = %v, want domain.ErrNotFound", err)
		}
		if store.requestedID != nil {
			t.Error("the store was asked to request a check for a missing monitor")
		}
	})

	t.Run("disabled monitor is rejected without touching the store", func(t *testing.T) {
		t.Parallel()

		id := uuid.New()
		store := newFakeStore()
		store.monitors[id] = domain.Monitor{ID: id, Enabled: false}
		svc := monitor.NewService(store, testDefaults())

		_, err := svc.RequestCheck(context.Background(), id)
		if !errors.Is(err, domain.ErrMonitorDisabled) {
			t.Errorf("error = %v, want domain.ErrMonitorDisabled", err)
		}
		if store.requestedID != nil {
			t.Error("the store was asked to request a check for a disabled monitor")
		}
	})
}

func TestListChecksPagination(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	makeChecks := func(monitorID uuid.UUID, n int) []domain.Check {
		checks := make([]domain.Check, 0, n)
		for i := range n {
			checks = append(checks, domain.Check{
				ID: int64(100 - i), MonitorID: monitorID,
				CheckedAt: base.Add(-time.Duration(i) * time.Minute),
			})
		}
		return checks
	}
	withMonitor := func() (*fakeStore, uuid.UUID) {
		id := uuid.New()
		store := newFakeStore()
		store.monitors[id] = domain.Monitor{ID: id, Enabled: true}
		return store, id
	}

	t.Run("full page yields a cursor", func(t *testing.T) {
		t.Parallel()

		store, id := withMonitor()
		store.checks = makeChecks(id, 4)
		svc := monitor.NewService(store, testDefaults())

		cursor := &monitor.CheckCursor{CheckedAt: base.Add(time.Hour), ID: 500}
		got, next, err := svc.ListChecks(context.Background(), id, 3, cursor)
		if err != nil {
			t.Fatalf("ListChecks() returned an unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("returned %d checks, want the requested 3", len(got))
		}
		if next == nil {
			t.Fatal("next cursor is nil, want a cursor because more rows exist")
		}
		if next.ID != got[2].ID || !next.CheckedAt.Equal(got[2].CheckedAt) {
			t.Error("cursor does not point at the last returned row")
		}
		p := store.listChecksParams
		if p.Limit != 4 {
			t.Errorf("store limit = %d, want limit+1", p.Limit)
		}
		if p.MonitorID != id {
			t.Errorf("store monitor id = %v, want %v", p.MonitorID, id)
		}
		if p.Cursor != cursor {
			t.Error("the caller's cursor did not reach the store")
		}
	})

	t.Run("partial page ends pagination", func(t *testing.T) {
		t.Parallel()

		store, id := withMonitor()
		store.checks = makeChecks(id, 2)
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.ListChecks(context.Background(), id, 3, nil)
		if err != nil {
			t.Fatalf("ListChecks() returned an unexpected error: %v", err)
		}
		if len(got) != 2 || next != nil {
			t.Errorf("got %d checks and cursor %v, want 2 and nil", len(got), next)
		}
	})

	t.Run("no checks is an empty page", func(t *testing.T) {
		t.Parallel()

		store, id := withMonitor()
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.ListChecks(context.Background(), id, 3, nil)
		if err != nil {
			t.Fatalf("ListChecks() returned an unexpected error: %v", err)
		}
		if len(got) != 0 || next != nil {
			t.Errorf("got %d checks and cursor %v, want none", len(got), next)
		}
	})

	t.Run("missing monitor is not found", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		svc := monitor.NewService(store, testDefaults())

		_, _, err := svc.ListChecks(context.Background(), uuid.New(), 3, nil)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("error = %v, want domain.ErrNotFound", err)
		}
		if store.listChecksParams != nil {
			t.Error("the store was queried for the history of a missing monitor")
		}
	})

	t.Run("limit is clamped", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct{ requested, want int32 }{
			{requested: 0, want: monitor.DefaultPageSize},
			{requested: 1000, want: monitor.MaxPageSize},
		} {
			store, id := withMonitor()
			svc := monitor.NewService(store, testDefaults())

			if _, _, err := svc.ListChecks(context.Background(), id, tc.requested, nil); err != nil {
				t.Fatalf("ListChecks() returned an unexpected error: %v", err)
			}
			if got := store.listChecksParams.Limit - 1; got != tc.want {
				t.Errorf("limit %d became %d, want %d", tc.requested, got, tc.want)
			}
		}
	})
}
