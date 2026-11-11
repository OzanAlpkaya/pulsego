//go:build integration

package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

func TestRequestCheck(t *testing.T) {
	store := newStore(t)

	m, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}
	if _, err := sharedPool.Exec(t.Context(),
		"UPDATE monitors SET next_check_at = now() + interval '1 hour' WHERE id = $1", m.ID); err != nil {
		t.Fatalf("pushing next_check_at out failed: %v", err)
	}

	got, err := store.RequestCheck(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("RequestCheck() returned an unexpected error: %v", err)
	}
	if got.ID != m.ID {
		t.Errorf("RequestCheck() returned monitor %v, want %v", got.ID, m.ID)
	}

	// Compare against PostgreSQL's clock, which is what the claim uses.
	var (
		stored time.Time
		isDue  bool
		recent bool
	)
	if err := sharedPool.QueryRow(t.Context(), `
		SELECT next_check_at, next_check_at <= now(), next_check_at > now() - interval '5 seconds'
		FROM monitors WHERE id = $1`, m.ID).Scan(&stored, &isDue, &recent); err != nil {
		t.Fatalf("reading next_check_at failed: %v", err)
	}
	if !isDue || !recent {
		t.Errorf("next_check_at = %v, want the database's now()", stored)
	}
	if !got.NextCheckAt.Equal(stored) {
		t.Errorf("returned next_check_at = %v, want the stored %v", got.NextCheckAt, stored)
	}
	if !got.UpdatedAt.Equal(m.UpdatedAt) {
		t.Errorf("updated_at changed from %v to %v; a check request is not an edit", m.UpdatedAt, got.UpdatedAt)
	}

	t.Run("deleted monitor is not found", func(t *testing.T) {
		if err := store.SoftDelete(t.Context(), m.ID); err != nil {
			t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
		}
		if _, err := store.RequestCheck(t.Context(), m.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("RequestCheck() error = %v, want domain.ErrNotFound", err)
		}
	})

	t.Run("missing monitor is not found", func(t *testing.T) {
		if _, err := store.RequestCheck(t.Context(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("RequestCheck() error = %v, want domain.ErrNotFound", err)
		}
	})
}

// insertCheck writes a check row directly, so these tests do not depend on the
// scheduler's lease-guarded write path.
func insertCheck(t *testing.T, monitorID uuid.UUID, checkedAt time.Time, success bool) int64 {
	t.Helper()

	var (
		statusCode = 200
		errorKind  *string
		message    *string
	)
	if !success {
		statusCode = 503
		kind, msg := string(domain.ErrorKindUnexpectedStatus), "status 503 outside expected range 200-399"
		errorKind, message = &kind, &msg
	}

	var id int64
	if err := sharedPool.QueryRow(t.Context(), `
		INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms,
		                            error_kind, error_message, checked_at)
		VALUES ($1, $2, $3, 42, $4, $5, $6)
		RETURNING id`,
		monitorID, success, statusCode, errorKind, message, checkedAt).Scan(&id); err != nil {
		t.Fatalf("inserting a check failed: %v", err)
	}
	return id
}

func TestListChecksKeysetPagination(t *testing.T) {
	store := newStore(t)

	m, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}
	other, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	// 25 checks over 10 distinct timestamps, so most pages have to break ties
	// on the id. Microsecond precision matches what PostgreSQL stores.
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	const total = 25
	inserted := make(map[int64]bool, total)
	for i := range total {
		at := base.Add(time.Duration(i%10) * time.Second)
		inserted[insertCheck(t, m.ID, at, i%3 != 0)] = false
	}
	// Another monitor's history must never leak into this one's.
	insertCheck(t, other.ID, base.Add(time.Minute), true)

	const pageSize = 7
	var (
		cursor *monitor.CheckCursor
		prev   *domain.Check
		pages  int
		seen   int
	)
	for {
		page, err := store.ListChecks(t.Context(), monitor.ListChecksParams{
			MonitorID: m.ID, Limit: pageSize, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("ListChecks() returned an unexpected error: %v", err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > pageSize {
			t.Fatalf("page has %d rows, want at most %d", len(page), pageSize)
		}

		for i := range page {
			c := page[i]
			already, known := inserted[c.ID]
			if !known {
				t.Fatalf("ListChecks() returned an unknown check %d (monitor %v)", c.ID, c.MonitorID)
			}
			if already {
				t.Errorf("check %d was returned on more than one page", c.ID)
			}
			inserted[c.ID] = true
			seen++

			if prev != nil && (c.CheckedAt.After(prev.CheckedAt) ||
				(c.CheckedAt.Equal(prev.CheckedAt) && c.ID >= prev.ID)) {
				t.Errorf("check %d (%v) follows %d (%v); want (checked_at, id) descending",
					c.ID, c.CheckedAt, prev.ID, prev.CheckedAt)
			}
			prev = &c

			if c.Success != (c.ErrorKind == nil) {
				t.Errorf("check %d: success = %v with error kind %v", c.ID, c.Success, c.ErrorKind)
			}
			if !c.Success && (*c.ErrorKind != domain.ErrorKindUnexpectedStatus || c.ErrorMessage == nil) {
				t.Errorf("check %d: failure fields not mapped: %+v", c.ID, c.CheckOutcome)
			}
		}

		last := page[len(page)-1]
		cursor = &monitor.CheckCursor{CheckedAt: last.CheckedAt, ID: last.ID}

		pages++
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}

	if seen != total {
		t.Errorf("saw %d checks across %d pages, want %d", seen, pages, total)
	}
	for id, wasSeen := range inserted {
		if !wasSeen {
			t.Errorf("check %d was never returned", id)
		}
	}
}

func TestListChecksEmptyReturnsEmptySlice(t *testing.T) {
	store := newStore(t)

	m, err := store.Create(t.Context(), validCreate())
	if err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	page, err := store.ListChecks(t.Context(), monitor.ListChecksParams{MonitorID: m.ID, Limit: 10})
	if err != nil {
		t.Fatalf("ListChecks() returned an unexpected error: %v", err)
	}
	if page == nil || len(page) != 0 {
		t.Errorf("ListChecks() = %#v, want an empty, non-nil slice", page)
	}
}
