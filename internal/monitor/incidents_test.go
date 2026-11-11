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

func TestListIncidentsPagination(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	makeIncidents := func(monitorID uuid.UUID, n int) []domain.Incident {
		incidents := make([]domain.Incident, 0, n)
		for i := range n {
			incidents = append(incidents, domain.Incident{
				ID: uuid.New(), MonitorID: monitorID,
				StartedAt: base.Add(-time.Duration(i) * time.Hour),
			})
		}
		return incidents
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
		store.incidents = makeIncidents(id, 4)
		svc := monitor.NewService(store, testDefaults())

		cursor := &monitor.IncidentCursor{StartedAt: base.Add(time.Hour), ID: uuid.New()}
		got, next, err := svc.ListIncidents(context.Background(), id, 3, cursor)
		if err != nil {
			t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("returned %d incidents, want the requested 3", len(got))
		}
		if next == nil {
			t.Fatal("next cursor is nil, want a cursor because more rows exist")
		}
		if next.ID != got[2].ID || !next.StartedAt.Equal(got[2].StartedAt) {
			t.Error("cursor does not point at the last returned row")
		}
		p := store.listIncidentsParams
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
		store.incidents = makeIncidents(id, 2)
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.ListIncidents(context.Background(), id, 3, nil)
		if err != nil {
			t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
		}
		if len(got) != 2 || next != nil {
			t.Errorf("got %d incidents and cursor %v, want 2 and nil", len(got), next)
		}
	})

	t.Run("no incidents is an empty page", func(t *testing.T) {
		t.Parallel()

		store, id := withMonitor()
		svc := monitor.NewService(store, testDefaults())

		got, next, err := svc.ListIncidents(context.Background(), id, 3, nil)
		if err != nil {
			t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
		}
		if len(got) != 0 || next != nil {
			t.Errorf("got %d incidents and cursor %v, want none", len(got), next)
		}
	})

	t.Run("missing monitor is not found", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		svc := monitor.NewService(store, testDefaults())

		_, _, err := svc.ListIncidents(context.Background(), uuid.New(), 3, nil)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("error = %v, want domain.ErrNotFound", err)
		}
		if store.listIncidentsParams != nil {
			t.Error("the store was queried for the incidents of a missing monitor")
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

			if _, _, err := svc.ListIncidents(context.Background(), id, tc.requested, nil); err != nil {
				t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
			}
			if got := store.listIncidentsParams.Limit - 1; got != tc.want {
				t.Errorf("limit %d became %d, want %d", tc.requested, got, tc.want)
			}
		}
	})
}
