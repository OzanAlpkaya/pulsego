//go:build integration

package repository_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// TestQAConcurrentRetentionCleanersDeleteEachRowOnce (Sprint 4 QA, D8): several
// workers may run retention at once. Concurrent cleaners never error, never
// delete a row twice (the per-call counts add up to exactly the expired rows),
// and never touch rows inside the retention period.
func TestQAConcurrentRetentionCleanersDeleteEachRowOnce(t *testing.T) {
	truncateMonitors(t)
	store := repository.NewRetentionStore(sharedPool)
	id := seedMonitors(t, dueSpec(1))[0]

	const expired = 6000
	if _, err := sharedPool.Exec(t.Context(), `
		INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms, checked_at)
		SELECT $1, true, 200, 5, now() - interval '40 days' - g * interval '1 second'
		FROM generate_series(1, $2::int) g`, id, expired); err != nil {
		t.Fatalf("seeding expired checks failed: %v", err)
	}
	keptID := insertCheck(t, id, dbNow(t).Add(-24*time.Hour), true)

	const (
		cleaners  = 6
		batchSize = 250
	)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		deleted int64
		errs    []error
		start   = make(chan struct{})
	)
	for range cleaners {
		wg.Go(func() {
			<-start
			for {
				n, err := store.DeleteExpiredChecks(t.Context(), 30, batchSize)
				mu.Lock()
				deleted += n
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
				if err != nil || n < batchSize {
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()

	// A cleaner may stop on a short batch while another still holds rows; a
	// final sequential pass (the next tick) picks up anything left.
	for {
		n, err := store.DeleteExpiredChecks(t.Context(), 30, batchSize)
		if err != nil {
			t.Fatalf("final pass failed: %v", err)
		}
		deleted += n
		if n < batchSize {
			break
		}
	}

	if len(errs) != 0 {
		t.Fatalf("concurrent cleaners returned errors: %v", errs)
	}
	if deleted != expired {
		t.Errorf("deleted rows reported = %d, want exactly %d", deleted, expired)
	}
	if got := countChecks(t, id); got != 1 {
		t.Errorf("checks left = %d, want 1 (the retained one)", got)
	}
	if !checkExists(t, keptID) {
		t.Error("a check inside the retention period was deleted")
	}
}

// TestQAIncidentHistoryEndpointAfterRecord (Sprint 4 QA, D3+D4) drives real
// check results through SchedulerStore.Record and reads them back through the
// real router: the incident Record opens and resolves is what the endpoint
// returns, newest first, with JSON nulls for an open incident; a monitor with
// no incidents is 200 with an empty array; a soft-deleted monitor is 404.
func TestQAIncidentHistoryEndpointAfterRecord(t *testing.T) {
	const (
		apiKey   = "integration-test-api-key-0123456789"
		workerID = "qa-worker"
	)
	sched := newSchedulerStore(t) // truncates
	store := repository.NewMonitorStore(sharedPool)
	ids := seedMonitors(t, dueSpec(2))
	id, quiet := ids[0], ids[1]

	router := httpapi.NewRouter(
		httpapi.RouterConfig{
			APIKey:   apiKey,
			Monitors: monitor.NewService(store, monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}, security.Policy{}),
		},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	get := func(t *testing.T, path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatalf("building the request failed: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the response failed: %v", err)
		}
		return resp.StatusCode, body
	}

	record := func(t *testing.T, outcome domain.CheckOutcome) domain.StateChange {
		t.Helper()
		claimed := armLease(t, id, workerID)
		change, err := sched.Record(t.Context(), workerID, id, claimed, outcome)
		if err != nil {
			t.Fatalf("Record() returned an unexpected error: %v", err)
		}
		return change
	}

	// Threshold seeded by dueSpec; drive the monitor to down, back up, and
	// down again: two incidents, the older resolved, the newer open.
	setState(t, id, domain.StateDegraded, seededThreshold-1)
	if c := record(t, failureOutcome()); c.Incident != domain.IncidentOpen {
		t.Fatalf("first failure at threshold: incident action = %v, want open", c.Incident)
	}
	firstOpen, _ := latestCheck(t, id)
	if c := record(t, successOutcome()); c.Incident != domain.IncidentResolve {
		t.Fatalf("recovery: incident action = %v, want resolve", c.Incident)
	}
	firstClose, _ := latestCheck(t, id)
	setState(t, id, domain.StateDegraded, seededThreshold-1)
	record(t, failureOutcome())
	secondOpen, _ := latestCheck(t, id)

	type incident struct {
		ID             uuid.UUID  `json:"id"`
		MonitorID      uuid.UUID  `json:"monitor_id"`
		StartedAt      time.Time  `json:"started_at"`
		ResolvedAt     *time.Time `json:"resolved_at"`
		OpeningCheckID *int64     `json:"opening_check_id"`
		ClosingCheckID *int64     `json:"closing_check_id"`
		Cause          *string    `json:"cause"`
	}
	type page struct {
		Data       []incident `json:"data"`
		NextCursor string     `json:"next_cursor"`
	}

	status, raw := get(t, "/v1/monitors/"+id.String()+"/incidents")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
	}
	var p page
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if len(p.Data) != 2 {
		t.Fatalf("got %d incidents, want 2 (body: %s)", len(p.Data), raw)
	}
	newest, oldest := p.Data[0], p.Data[1]
	if newest.ResolvedAt != nil || newest.ClosingCheckID != nil {
		t.Errorf("newest incident should be open, got resolved_at=%v closing=%v", newest.ResolvedAt, deref(newest.ClosingCheckID))
	}
	if newest.OpeningCheckID == nil || *newest.OpeningCheckID != secondOpen {
		t.Errorf("newest opening_check_id = %v, want %d", deref(newest.OpeningCheckID), secondOpen)
	}
	if oldest.OpeningCheckID == nil || *oldest.OpeningCheckID != firstOpen ||
		oldest.ClosingCheckID == nil || *oldest.ClosingCheckID != firstClose || oldest.ResolvedAt == nil {
		t.Errorf("oldest incident = open %v close %v resolved %v, want open %d close %d resolved",
			deref(oldest.OpeningCheckID), deref(oldest.ClosingCheckID), oldest.ResolvedAt, firstOpen, firstClose)
	}
	if !newest.StartedAt.After(oldest.StartedAt) {
		t.Errorf("incidents not newest first: %v then %v", newest.StartedAt, oldest.StartedAt)
	}
	for _, inc := range p.Data {
		if inc.MonitorID != id || inc.Cause == nil || *inc.Cause == "" {
			t.Errorf("incident %v: monitor_id %v cause %v", inc.ID, inc.MonitorID, deref(inc.Cause))
		}
	}

	// The open incident's nullable fields are JSON null, not omitted.
	var rawPage struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &rawPage); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	for _, k := range []string{"resolved_at", "closing_check_id"} {
		if v, present := rawPage.Data[0][k]; !present || v != nil {
			t.Errorf("open incident %s = %v (present %v), want null", k, v, present)
		}
	}

	t.Run("limit 1 pages through both", func(t *testing.T) {
		status, raw := get(t, "/v1/monitors/"+id.String()+"/incidents?limit=1")
		var first page
		if status != http.StatusOK || json.Unmarshal(raw, &first) != nil || len(first.Data) != 1 || first.NextCursor == "" {
			t.Fatalf("first page: status %d body %s", status, raw)
		}
		status, raw = get(t, "/v1/monitors/"+id.String()+"/incidents?limit=1&cursor="+first.NextCursor)
		var second page
		if status != http.StatusOK || json.Unmarshal(raw, &second) != nil || len(second.Data) != 1 {
			t.Fatalf("second page: status %d body %s", status, raw)
		}
		if second.Data[0].ID != oldest.ID {
			t.Errorf("second page = %v, want %v", second.Data[0].ID, oldest.ID)
		}
	})

	t.Run("no incidents is 200 with empty array", func(t *testing.T) {
		status, raw := get(t, "/v1/monitors/"+quiet.String()+"/incidents")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if data, ok := body["data"].([]any); !ok || len(data) != 0 {
			t.Errorf("data = %#v, want []", body["data"])
		}
	})

	t.Run("soft-deleted monitor is 404", func(t *testing.T) {
		if err := store.SoftDelete(t.Context(), id); err != nil {
			t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
		}
		if status, raw := get(t, "/v1/monitors/"+id.String()+"/incidents"); status != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (body: %s)", status, raw)
		}
	})
}
