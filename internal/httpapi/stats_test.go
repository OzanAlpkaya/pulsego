package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
)

const statsPath = "/v1/monitors/11111111-2222-3333-4444-555555555555/stats"

// sampleStats returns what the service would compute for window: a fractional
// uptime and average, so the wire format of both is exercised.
func sampleStats(window domain.StatsWindow) domain.Stats {
	end := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	uptime, avg, p95 := 71.43, 400.2, int32(1000)
	return domain.Stats{
		Window: window, From: end.Add(-window.Duration()), To: end,
		TotalChecks: 7, SuccessfulChecks: 5,
		UptimePercent: &uptime, AvgLatencyMs: &avg, P95LatencyMs: &p95,
	}
}

func TestMonitorStatsEndpoint(t *testing.T) {
	t.Parallel()

	for _, window := range domain.StatsWindows {
		t.Run("window "+window.String(), func(t *testing.T) {
			t.Parallel()

			svc := &stubService{stats: sampleStats(window)}
			rec := httptest.NewRecorder()
			newTestRouter(t, svc).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, statsPath+"?window="+window.String(), nil))

			if got, want := rec.Code, http.StatusOK; got != want {
				t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if svc.gotStatsWindow != window {
				t.Errorf("service received window %q, want %q", svc.gotStatsWindow, window)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			from := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC).Add(-window.Duration())
			want := map[string]any{
				"window":            window.String(),
				"from":              from.Format(time.RFC3339),
				"to":                "2026-11-11T12:00:00Z",
				"total_checks":      float64(7),
				"successful_checks": float64(5),
				"uptime_percent":    71.43,
				"avg_latency_ms":    400.2,
				"p95_latency_ms":    float64(1000),
			}
			if len(body) != len(want) {
				t.Errorf("response has %d fields, want %d: %v", len(body), len(want), body)
			}
			for k, v := range want {
				if body[k] != v {
					t.Errorf("%s = %v, want %v", k, body[k], v)
				}
			}
		})
	}

	t.Run("window defaults to 24h", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{stats: sampleStats(domain.StatsWindow24h)}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, statsPath, nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
		}
		if svc.gotStatsWindow != domain.StatsWindow24h {
			t.Errorf("service received window %q, want 24h", svc.gotStatsWindow)
		}
	})

	t.Run("no data serialises as null", func(t *testing.T) {
		t.Parallel()

		end := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
		svc := &stubService{stats: domain.Stats{
			Window: domain.StatsWindow24h, From: end.Add(-24 * time.Hour), To: end,
		}}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, statsPath, nil))

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		for _, k := range []string{"uptime_percent", "avg_latency_ms", "p95_latency_ms"} {
			v, present := body[k]
			if !present || v != nil {
				t.Errorf("%s = %v (present %v), want null", k, v, present)
			}
		}
		if body["total_checks"] != float64(0) || body["successful_checks"] != float64(0) {
			t.Errorf("counts = %v/%v, want 0/0", body["successful_checks"], body["total_checks"])
		}
	})

	t.Run("bad window is a 400 validation problem naming window", func(t *testing.T) {
		t.Parallel()

		for _, raw := range []string{"1d", "24H", "90d", "7", " 7d"} {
			svc := &stubService{}
			rec := httptest.NewRecorder()
			newTestRouter(t, svc).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, statsPath+"?window="+url.QueryEscape(raw), nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Fatalf("window %q: status = %d, want %d", raw, got, want)
			}
			p := decodeProblem(t, rec)
			if p.Type != httpapi.TypeValidation {
				t.Errorf("window %q: type = %q, want %q", raw, p.Type, httpapi.TypeValidation)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != "window" {
				t.Errorf("window %q: errors = %+v, want one entry for window", raw, p.Errors)
			}
			if svc.gotStatsWindow != "" {
				t.Errorf("window %q reached the service", raw)
			}
		}
	})

	t.Run("missing monitor is 404", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{err: domain.ErrNotFound}).ServeHTTP(rec,
			authedRequest(t, http.MethodGet, statsPath+"?window=7d", nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if p := decodeProblem(t, rec); p.Type != httpapi.TypeNotFound {
			t.Errorf("type = %q, want %q", p.Type, httpapi.TypeNotFound)
		}
	})

	t.Run("malformed id is 400", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec,
			authedRequest(t, http.MethodGet, "/v1/monitors/not-a-uuid/stats", nil))

		if got, want := rec.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if svc.gotStatsWindow != "" {
			t.Error("the request reached the service despite a malformed id")
		}
	})
}
