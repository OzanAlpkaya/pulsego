//go:build integration

package repository_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
)

// insertStatsCheck writes a check row with an explicit latency and
// checked_at, so the expected aggregates are known exactly.
func insertStatsCheck(t *testing.T, monitorID uuid.UUID, checkedAt time.Time, success bool, latencyMS int32) {
	t.Helper()

	var (
		statusCode = 200
		errorKind  *string
	)
	if !success {
		statusCode = 503
		kind := string(domain.ErrorKindUnexpectedStatus)
		errorKind = &kind
	}

	if _, err := sharedPool.Exec(t.Context(), `
		INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms, error_kind, checked_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		monitorID, success, statusCode, latencyMS, errorKind, checkedAt); err != nil {
		t.Fatalf("inserting a check failed: %v", err)
	}
}

// statsWant is the exact expected aggregate of one window of the fixture.
type statsWant struct {
	total, successful int64
	rawAvg            float64
	avg, uptime       float64
	p95               int32
}

// statsFixture holds the monitors seedStatsFixture creates.
type statsFixture struct {
	main, other, empty, allFailed uuid.UUID
}

// statsExpected is what the fixture's main monitor must report. Each window
// adds the checks of the next age bucket:
//
//	~1h ago:  ok 100, ok 200, ok 301, fail 9000
//	~2d ago:  ok 400, ok 1000, fail 1
//	~10d ago: ok 2000, fail 9000, fail 9000
//	~40d ago: ok 5, fail 9000 (outside every window)
//
// Failed latencies are extreme on purpose: counting one in the average or
// the p95 would move it visibly.
var statsExpected = map[domain.StatsWindow]statsWant{
	// [100 200 301]: p95 is the 3rd of 3.
	domain.StatsWindow24h: {total: 4, successful: 3, rawAvg: 601.0 / 3, avg: 200.33, uptime: 75, p95: 301},
	// [100 200 301 400 1000]: p95 is the 5th of 5.
	domain.StatsWindow7d: {total: 7, successful: 5, rawAvg: 400.2, avg: 400.2, uptime: 71.43, p95: 1000},
	// [100 200 301 400 1000 2000]: p95 is the 6th of 6.
	domain.StatsWindow30d: {total: 10, successful: 6, rawAvg: 4001.0 / 6, avg: 666.83, uptime: 60, p95: 2000},
}

// seedStatsFixture creates the monitors and checks described on
// statsExpected. Ages are relative to the database's now() and far from
// every window boundary, so the test cannot flake on clock movement.
func seedStatsFixture(t *testing.T, store *repository.MonitorStore) statsFixture {
	t.Helper()

	create := func() uuid.UUID {
		m, err := store.Create(t.Context(), validCreate())
		if err != nil {
			t.Fatalf("Create() returned an unexpected error: %v", err)
		}
		return m.ID
	}
	f := statsFixture{main: create(), other: create(), empty: create(), allFailed: create()}

	now := dbNow(t)
	hour, twoDays := now.Add(-time.Hour), now.Add(-48*time.Hour)
	tenDays, fortyDays := now.Add(-10*24*time.Hour), now.Add(-40*24*time.Hour)

	for _, c := range []struct {
		at      time.Time
		success bool
		latency int32
	}{
		{hour, true, 100}, {hour.Add(time.Minute), true, 200}, {hour.Add(2 * time.Minute), true, 301},
		{hour.Add(3 * time.Minute), false, 9000},
		{twoDays, true, 400}, {twoDays.Add(time.Minute), true, 1000}, {twoDays.Add(2 * time.Minute), false, 1},
		{tenDays, true, 2000}, {tenDays.Add(time.Minute), false, 9000}, {tenDays.Add(2 * time.Minute), false, 9000},
		{fortyDays, true, 5}, {fortyDays.Add(time.Minute), false, 9000},
	} {
		insertStatsCheck(t, f.main, c.at, c.success, c.latency)
	}

	// Another monitor's checks, in every window, must not leak in.
	for _, at := range []time.Time{hour, twoDays, tenDays} {
		insertStatsCheck(t, f.other, at, true, 7)
		insertStatsCheck(t, f.other, at, false, 7)
	}

	insertStatsCheck(t, f.allFailed, hour, false, 5000)
	insertStatsCheck(t, f.allFailed, tenDays, false, 5000)

	return f
}

func TestMonitorStatsWindows(t *testing.T) {
	store := newStore(t)
	f := seedStatsFixture(t, store)

	for _, window := range domain.StatsWindows {
		t.Run(window.String(), func(t *testing.T) {
			want := statsExpected[window]

			before := dbNow(t)
			raw, err := store.GetStats(t.Context(), f.main, window)
			if err != nil {
				t.Fatalf("GetStats() returned an unexpected error: %v", err)
			}
			if raw.TotalChecks != want.total || raw.SuccessfulChecks != want.successful {
				t.Errorf("counts = %d/%d, want %d/%d", raw.SuccessfulChecks, raw.TotalChecks, want.successful, want.total)
			}
			if raw.AvgLatencyMs == nil || math.Abs(*raw.AvgLatencyMs-want.rawAvg) > 1e-9 {
				t.Errorf("raw AvgLatencyMs = %v, want %v", deref(raw.AvgLatencyMs), want.rawAvg)
			}
			if raw.P95LatencyMs == nil || *raw.P95LatencyMs != want.p95 {
				t.Errorf("P95LatencyMs = %v, want %d", deref(raw.P95LatencyMs), want.p95)
			}
			if raw.WindowEnd.Before(before) || raw.WindowEnd.After(dbNow(t)) {
				t.Errorf("WindowEnd = %v, want the database's now()", raw.WindowEnd)
			}

			stats := domain.NewStats(window, raw)
			if stats.UptimePercent == nil || *stats.UptimePercent != want.uptime {
				t.Errorf("UptimePercent = %v, want %v", deref(stats.UptimePercent), want.uptime)
			}
			if stats.AvgLatencyMs == nil || *stats.AvgLatencyMs != want.avg {
				t.Errorf("AvgLatencyMs = %v, want %v", deref(stats.AvgLatencyMs), want.avg)
			}
		})
	}

	t.Run("other monitor sees only its own checks", func(t *testing.T) {
		raw, err := store.GetStats(t.Context(), f.other, domain.StatsWindow30d)
		if err != nil {
			t.Fatalf("GetStats() returned an unexpected error: %v", err)
		}
		if raw.TotalChecks != 6 || raw.SuccessfulChecks != 3 {
			t.Errorf("counts = %d/%d, want 3/6", raw.SuccessfulChecks, raw.TotalChecks)
		}
		if raw.P95LatencyMs == nil || *raw.P95LatencyMs != 7 {
			t.Errorf("P95LatencyMs = %v, want 7", deref(raw.P95LatencyMs))
		}
	})

	t.Run("empty monitor gives zeros and nulls", func(t *testing.T) {
		for _, window := range domain.StatsWindows {
			raw, err := store.GetStats(t.Context(), f.empty, window)
			if err != nil {
				t.Fatalf("GetStats(%s) returned an unexpected error: %v", window, err)
			}
			if raw.TotalChecks != 0 || raw.SuccessfulChecks != 0 || raw.AvgLatencyMs != nil || raw.P95LatencyMs != nil {
				t.Errorf("%s: raw = %+v, want zeros and nulls", window, raw)
			}
			if stats := domain.NewStats(window, raw); stats.UptimePercent != nil {
				t.Errorf("%s: UptimePercent = %v, want nil", window, *stats.UptimePercent)
			}
		}
	})

	t.Run("all failed gives zero uptime and null latency", func(t *testing.T) {
		raw, err := store.GetStats(t.Context(), f.allFailed, domain.StatsWindow30d)
		if err != nil {
			t.Fatalf("GetStats() returned an unexpected error: %v", err)
		}
		if raw.TotalChecks != 2 || raw.SuccessfulChecks != 0 || raw.AvgLatencyMs != nil || raw.P95LatencyMs != nil {
			t.Errorf("raw = %+v, want 2 checks, none successful, null latency", raw)
		}
		stats := domain.NewStats(domain.StatsWindow30d, raw)
		if stats.UptimePercent == nil || *stats.UptimePercent != 0 {
			t.Errorf("UptimePercent = %v, want 0", deref(stats.UptimePercent))
		}
	})
}

// TestMonitorStatsEndpoint drives GET /v1/monitors/{id}/stats through the
// real router, service and store, for every window.
func TestMonitorStatsEndpoint(t *testing.T) {
	const apiKey = "integration-test-api-key-0123456789"

	store := newStore(t)
	f := seedStatsFixture(t, store)
	router := httpapi.NewRouter(
		httpapi.RouterConfig{
			APIKey:   apiKey,
			Monitors: monitor.NewService(store, monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}),
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
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("closing the response body failed: %v", closeErr)
		}
		if err != nil {
			t.Fatalf("reading the response failed: %v", err)
		}
		return resp.StatusCode, body
	}

	type statsBody struct {
		Window           string    `json:"window"`
		From             time.Time `json:"from"`
		To               time.Time `json:"to"`
		TotalChecks      int64     `json:"total_checks"`
		SuccessfulChecks int64     `json:"successful_checks"`
		UptimePercent    *float64  `json:"uptime_percent"`
		AvgLatencyMs     *float64  `json:"avg_latency_ms"`
		P95LatencyMs     *int32    `json:"p95_latency_ms"`
	}

	for _, window := range domain.StatsWindows {
		t.Run(window.String(), func(t *testing.T) {
			want := statsExpected[window]

			status, raw := get(t, "/v1/monitors/"+f.main.String()+"/stats?window="+window.String())
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
			}
			var body statsBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}

			if body.Window != window.String() {
				t.Errorf("window = %q, want %q", body.Window, window)
			}
			if got := body.To.Sub(body.From); got != window.Duration() {
				t.Errorf("to - from = %v, want %v", got, window.Duration())
			}
			if body.TotalChecks != want.total || body.SuccessfulChecks != want.successful {
				t.Errorf("counts = %d/%d, want %d/%d", body.SuccessfulChecks, body.TotalChecks, want.successful, want.total)
			}
			if body.UptimePercent == nil || *body.UptimePercent != want.uptime {
				t.Errorf("uptime_percent = %v, want %v", deref(body.UptimePercent), want.uptime)
			}
			if body.AvgLatencyMs == nil || *body.AvgLatencyMs != want.avg {
				t.Errorf("avg_latency_ms = %v, want %v", deref(body.AvgLatencyMs), want.avg)
			}
			if body.P95LatencyMs == nil || *body.P95LatencyMs != want.p95 {
				t.Errorf("p95_latency_ms = %v, want %d", deref(body.P95LatencyMs), want.p95)
			}
		})
	}

	t.Run("default window is 24h", func(t *testing.T) {
		status, raw := get(t, "/v1/monitors/"+f.main.String()+"/stats")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
		}
		var body statsBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if body.Window != "24h" || body.TotalChecks != statsExpected[domain.StatsWindow24h].total {
			t.Errorf("window %q with %d checks, want 24h with %d", body.Window, body.TotalChecks,
				statsExpected[domain.StatsWindow24h].total)
		}
	})

	t.Run("empty monitor serialises nulls", func(t *testing.T) {
		status, raw := get(t, "/v1/monitors/"+f.empty.String()+"/stats?window=30d")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", status, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		for _, k := range []string{"uptime_percent", "avg_latency_ms", "p95_latency_ms"} {
			if v, present := body[k]; !present || v != nil {
				t.Errorf("%s = %v (present %v), want null", k, v, present)
			}
		}
	})

	t.Run("bad window is 400", func(t *testing.T) {
		if status, raw := get(t, "/v1/monitors/"+f.main.String()+"/stats?window=1d"); status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body: %s)", status, raw)
		}
	})

	t.Run("missing and deleted monitors are 404", func(t *testing.T) {
		if err := store.SoftDelete(t.Context(), f.other); err != nil {
			t.Fatalf("SoftDelete() returned an unexpected error: %v", err)
		}
		for _, id := range []uuid.UUID{uuid.New(), f.other} {
			if status, raw := get(t, "/v1/monitors/"+id.String()+"/stats"); status != http.StatusNotFound {
				t.Errorf("%v: status = %d, want 404 (body: %s)", id, status, raw)
			}
		}
	})
}
