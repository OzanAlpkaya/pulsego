//go:build integration

package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/repository"
)

// checkExists reports whether a monitor_checks row with id is still there.
func checkExists(t *testing.T, id int64) bool {
	t.Helper()

	var exists bool
	if err := sharedPool.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM monitor_checks WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("reading check %d failed: %v", id, err)
	}
	return exists
}

// TestDeleteExpiredChecks (D8, D11): only checks older than the retention
// period are deleted, oldest first, at most batchSize per call; incidents
// that pointed at a deleted check keep their row with the reference NULL.
func TestDeleteExpiredChecks(t *testing.T) {
	truncateMonitors(t)
	store := repository.NewRetentionStore(sharedPool)

	const retentionDays = 30
	monitorIDs := seedMonitors(t, dueSpec(2))
	now := dbNow(t)
	day := 24 * time.Hour

	// Five expired checks across two monitors, aged 31..35 days, and kept
	// checks well inside the period (the boundary itself is not tested: the
	// cut-off moves with now()).
	var expired []int64 // oldest first
	for i := range 5 {
		age := time.Duration(35-i) * day
		expired = append(expired, insertCheck(t, monitorIDs[i%2], now.Add(-age), i%2 == 0))
	}
	kept := []int64{
		insertCheck(t, monitorIDs[0], now.Add(-29*day), true),
		insertCheck(t, monitorIDs[1], now.Add(-time.Hour), false),
		insertCheck(t, monitorIDs[0], now, true),
	}

	// A resolved incident whose opening and closing checks both expire, and an
	// open incident whose opening check is kept.
	var resolvedID, openID uuid.UUID
	if err := sharedPool.QueryRow(t.Context(), `
		INSERT INTO incidents (monitor_id, started_at, resolved_at, opening_check_id, closing_check_id, cause)
		VALUES ($1, $2, $3, $4, $5, 'timeout')
		RETURNING id`,
		monitorIDs[0], now.Add(-35*day), now.Add(-33*day), expired[0], expired[2]).Scan(&resolvedID); err != nil {
		t.Fatalf("inserting the resolved incident failed: %v", err)
	}
	if err := sharedPool.QueryRow(t.Context(), `
		INSERT INTO incidents (monitor_id, started_at, opening_check_id, cause)
		VALUES ($1, $2, $3, 'unexpected_status')
		RETURNING id`,
		monitorIDs[1], now.Add(-time.Hour), kept[1]).Scan(&openID); err != nil {
		t.Fatalf("inserting the open incident failed: %v", err)
	}

	// Batch size is respected, and the oldest go first.
	n, err := store.DeleteExpiredChecks(t.Context(), retentionDays, 2)
	if err != nil {
		t.Fatalf("DeleteExpiredChecks() returned an unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("first batch deleted %d rows, want 2", n)
	}
	for i, id := range expired {
		if got, want := checkExists(t, id), i >= 2; got != want {
			t.Errorf("after the first batch, expired check %d (age %d days) exists = %v, want %v",
				id, 35-i, got, want)
		}
	}

	// A batch larger than what is left deletes the rest and comes back short.
	n, err = store.DeleteExpiredChecks(t.Context(), retentionDays, 10)
	if err != nil {
		t.Fatalf("DeleteExpiredChecks() returned an unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("second batch deleted %d rows, want the 3 remaining expired", n)
	}

	// Nothing expired is left.
	n, err = store.DeleteExpiredChecks(t.Context(), retentionDays, 10)
	if err != nil {
		t.Fatalf("DeleteExpiredChecks() returned an unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("third batch deleted %d rows, want 0", n)
	}

	for _, id := range expired {
		if checkExists(t, id) {
			t.Errorf("expired check %d still exists", id)
		}
	}
	for _, id := range kept {
		if !checkExists(t, id) {
			t.Errorf("check %d inside the retention period was deleted", id)
		}
	}
	if got := countChecks(t, monitorIDs[0]) + countChecks(t, monitorIDs[1]); got != len(kept) {
		t.Errorf("%d checks remain, want %d", got, len(kept))
	}

	// Incidents keep their rows; only references to deleted checks are NULL.
	type incidentRefs struct {
		opening, closing *int64
		resolved         bool
		cause            *string
	}
	readRefs := func(id uuid.UUID) incidentRefs {
		t.Helper()
		var r incidentRefs
		if err := sharedPool.QueryRow(t.Context(), `
			SELECT opening_check_id, closing_check_id, resolved_at IS NOT NULL, cause
			FROM incidents WHERE id = $1`, id).Scan(&r.opening, &r.closing, &r.resolved, &r.cause); err != nil {
			t.Fatalf("reading incident %v failed: %v", id, err)
		}
		return r
	}

	resolved := readRefs(resolvedID)
	if resolved.opening != nil || resolved.closing != nil {
		t.Errorf("resolved incident references = (%v, %v), want both NULL", resolved.opening, resolved.closing)
	}
	if !resolved.resolved || resolved.cause == nil || *resolved.cause != "timeout" {
		t.Errorf("resolved incident = %+v, want it still resolved with its cause", resolved)
	}

	open := readRefs(openID)
	if open.opening == nil || *open.opening != kept[1] {
		t.Errorf("open incident opening_check_id = %v, want the kept check %d", open.opening, kept[1])
	}
	if open.resolved {
		t.Error("open incident became resolved")
	}
	if got := countIncidents(t, monitorIDs[0], false) + countIncidents(t, monitorIDs[1], false); got != 2 {
		t.Errorf("%d incidents remain, want 2", got)
	}
}
