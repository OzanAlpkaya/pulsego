//go:build integration

package repository_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// insertHistoryIncident writes an incident row directly, with every column
// set explicitly, and returns its id. resolvedAt nil leaves it open.
func insertHistoryIncident(t *testing.T, monitorID uuid.UUID, startedAt time.Time, resolvedAt *time.Time,
	openingCheckID, closingCheckID *int64, cause *string,
) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := sharedPool.QueryRow(t.Context(), `
		INSERT INTO incidents (monitor_id, started_at, resolved_at, opening_check_id, closing_check_id, cause)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		monitorID, startedAt, resolvedAt, openingCheckID, closingCheckID, cause).Scan(&id); err != nil {
		t.Fatalf("inserting an incident failed: %v", err)
	}
	return id
}

func TestListIncidentsKeysetPagination(t *testing.T) {
	store := newStore(t)

	m, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}
	other, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	// 17 resolved incidents over 6 distinct start times, so most pages have to
	// break ties on the id, plus one open incident newest of all.
	// Microsecond precision matches what PostgreSQL stores.
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-24 * time.Hour)
	const resolvedCount = 17
	inserted := make(map[uuid.UUID]bool, resolvedCount+1)
	for i := range resolvedCount {
		started := base.Add(time.Duration(i%6) * time.Minute)
		resolved := started.Add(30 * time.Second)
		cause := "timeout"
		inserted[insertHistoryIncident(t, m.ID, started, &resolved, nil, nil, &cause)] = false
	}

	// The open incident references real checks, so the mapping of every
	// column is checked once.
	openStarted := base.Add(time.Hour)
	openingCheck := insertCheck(t, m.ID, openStarted, false)
	cause := "unexpected_status: status 503 outside expected range 200-399"
	openID := insertHistoryIncident(t, m.ID, openStarted, nil, &openingCheck, nil, &cause)
	inserted[openID] = false
	total := len(inserted)

	// Another monitor's incidents must never leak into this one's.
	otherResolved := base.Add(2 * time.Hour)
	insertHistoryIncident(t, other.ID, base.Add(90*time.Minute), &otherResolved, nil, nil, nil)

	const pageSize = 5
	var (
		cursor *monitor.IncidentCursor
		prev   *domain.Incident
		pages  int
		seen   int
	)
	for {
		page, err := store.ListIncidents(t.Context(), monitor.ListIncidentsParams{
			MonitorID: m.ID, Limit: pageSize, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > pageSize {
			t.Fatalf("page has %d rows, want at most %d", len(page), pageSize)
		}

		for i := range page {
			inc := page[i]
			already, known := inserted[inc.ID]
			if !known {
				t.Fatalf("ListIncidents() returned an unknown incident %v (monitor %v)", inc.ID, inc.MonitorID)
			}
			if already {
				t.Errorf("incident %v was returned on more than one page", inc.ID)
			}
			inserted[inc.ID] = true
			seen++

			if prev != nil && (inc.StartedAt.After(prev.StartedAt) ||
				(inc.StartedAt.Equal(prev.StartedAt) && bytes.Compare(inc.ID[:], prev.ID[:]) >= 0)) {
				t.Errorf("incident %v (%v) follows %v (%v); want (started_at, id) descending (uuids compare bytewise, as in PostgreSQL)",
					inc.ID, inc.StartedAt, prev.ID, prev.StartedAt)
			}
			prev = &inc

			if inc.MonitorID != m.ID {
				t.Errorf("incident %v has monitor %v, want %v", inc.ID, inc.MonitorID, m.ID)
			}
			if inc.CreatedAt.IsZero() {
				t.Errorf("incident %v: created_at not mapped", inc.ID)
			}
			if inc.ID == openID {
				if pages != 0 || i != 0 {
					t.Errorf("the newest incident is at page %d row %d, want the first row", pages, i)
				}
				if inc.ResolvedAt != nil || inc.ClosingCheckID != nil {
					t.Errorf("open incident has resolution fields: %+v", inc)
				}
				if inc.OpeningCheckID == nil || *inc.OpeningCheckID != openingCheck {
					t.Errorf("opening_check_id = %v, want %d", inc.OpeningCheckID, openingCheck)
				}
				if inc.Cause == nil || *inc.Cause != cause {
					t.Errorf("cause = %v, want %q", inc.Cause, cause)
				}
				if !inc.StartedAt.Equal(openStarted) {
					t.Errorf("started_at = %v, want %v", inc.StartedAt, openStarted)
				}
			} else {
				if inc.ResolvedAt == nil || !inc.ResolvedAt.Equal(inc.StartedAt.Add(30*time.Second)) {
					t.Errorf("incident %v: resolved_at = %v, want started_at + 30s", inc.ID, inc.ResolvedAt)
				}
				if inc.OpeningCheckID != nil || inc.ClosingCheckID != nil {
					t.Errorf("incident %v: check references = %v/%v, want null", inc.ID, inc.OpeningCheckID, inc.ClosingCheckID)
				}
			}
		}

		last := page[len(page)-1]
		cursor = &monitor.IncidentCursor{StartedAt: last.StartedAt, ID: last.ID}

		pages++
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}

	if seen != total {
		t.Errorf("saw %d incidents across %d pages, want %d", seen, pages, total)
	}
	for id, wasSeen := range inserted {
		if !wasSeen {
			t.Errorf("incident %v was never returned", id)
		}
	}
}

func TestListIncidentsEmptyReturnsEmptySlice(t *testing.T) {
	store := newStore(t)

	m, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	page, err := store.ListIncidents(t.Context(), monitor.ListIncidentsParams{MonitorID: m.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListIncidents() returned an unexpected error: %v", err)
	}
	if page == nil || len(page) != 0 {
		t.Errorf("ListIncidents() = %#v, want an empty, non-nil slice", page)
	}
}
