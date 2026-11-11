package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// allMetricNames are the ten metrics fixed by the Sprint 5 plan (D3).
var allMetricNames = []string{
	"pulsego_http_requests_total",
	"pulsego_http_request_duration_seconds",
	"pulsego_monitor_checks_total",
	"pulsego_monitor_check_duration_seconds",
	"pulsego_monitor_state_total",
	"pulsego_worker_claimed_monitors_total",
	"pulsego_worker_active_checks",
	"pulsego_worker_claim_errors_total",
	"pulsego_scheduler_lag_seconds",
	"pulsego_db_operation_duration_seconds",
}

func kindPtr(k domain.ErrorKind) *domain.ErrorKind { return &k }

// gatherPulseGo returns every pulsego_* family by name.
func gatherPulseGo(t *testing.T, m *telemetry.Metrics) map[string]int {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	series := make(map[string]int)
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "pulsego_") {
			series[f.GetName()] = len(f.GetMetric())
		}
	}
	return series
}

func TestMetricsAllTenRegistered(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	m.ObserveHTTPRequest(http.MethodGet, "/v1/monitors", http.StatusOK, 5*time.Millisecond)
	m.ObserveCheck(true, nil, 20*time.Millisecond)
	m.StateChanged(domain.StateUnknown, domain.StateUp)
	m.AddClaimed(3)
	m.ActiveChecksInc()
	m.ClaimError()
	m.ObserveSchedulerLag(time.Second)
	m.ObserveDBOperation("GetMonitor", nil, time.Millisecond)

	series := gatherPulseGo(t, m)
	for _, name := range allMetricNames {
		if series[name] == 0 {
			t.Errorf("metric %s is not exposed", name)
		}
	}
	if len(series) != len(allMetricNames) {
		t.Errorf("exposed %d pulsego_* metrics, want exactly %d: %v", len(series), len(allMetricNames), series)
	}

	// The private registry also carries the runtime and process collectors.
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	var sawGo bool
	for _, f := range families {
		if f.GetName() == "go_goroutines" {
			sawGo = true
		}
	}
	if !sawGo {
		t.Error("go_goroutines missing: Go collector not registered")
	}
}

func TestMetricsHandlerServesExposition(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	m.AddClaimed(2)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "pulsego_worker_claimed_monitors_total 2") {
		t.Errorf("exposition lacks the claimed counter:\n%s", body)
	}
}

var (
	uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	urlPattern  = regexp.MustCompile(`://|\?`)
)

// TestMetricsCardinalityBounded feeds thousands of distinct inputs — request
// paths, IDs, methods, error kinds, states, query texts — and asserts the
// series count stays fixed and no label value carries an ID or a URL.
func TestMetricsCardinalityBounded(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	baseline := gatherPulseGo(t, m)

	const n = 5000
	routes := []string{"/v1/monitors", "/v1/monitors/{id}", "/healthz"}
	for i := range n {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)

		// A matched route arrives as its pattern; an unmatched path as "".
		m.ObserveHTTPRequest(http.MethodGet, routes[i%len(routes)], http.StatusOK, time.Millisecond)
		m.ObserveHTTPRequest(http.MethodGet, "", http.StatusNotFound, time.Millisecond)
		// Misuse: a raw path or URL instead of a pattern.
		m.ObserveHTTPRequest(fmt.Sprintf("M%d", i), "http://evil.example/"+id+"?q="+id, 999+i, time.Millisecond)
		m.ObserveHTTPRequest("get", "monitors/"+id, -i, time.Millisecond)

		m.ObserveCheck(false, kindPtr(domain.ErrorKind("kind-"+id)), time.Millisecond)
		m.ObserveCheck(false, nil, time.Millisecond)
		m.ObserveCheck(true, kindPtr(domain.ErrorKind(id)), time.Millisecond)

		m.StateChanged(domain.State("s-"+id), domain.State("t-"+id))
		m.StateChanged(domain.StateUp, domain.State(id))

		m.ObserveDBOperation("SELECT * FROM monitors WHERE id = '"+id+"'", errors.New(id), time.Millisecond)
		m.ObserveDBOperation(telemetry.OperationName("-- name: Get"+id+" :one\nSELECT 1"), nil, time.Millisecond)
	}

	got := gatherPulseGo(t, m)

	// Fixed upper bounds per family, independent of n.
	limits := map[string]int{
		"pulsego_http_requests_total":            (len(routes) + 1) * 2 * 3, // routes+unmatched × {GET,OTHER} × {200,404,unknown}
		"pulsego_http_request_duration_seconds":  (len(routes) + 1) * 2,
		"pulsego_monitor_checks_total":           1 + len(domain.ErrorKinds()),
		"pulsego_monitor_check_duration_seconds": 2,
		"pulsego_monitor_state_total":            len(domain.States()) * len(domain.States()),
		"pulsego_db_operation_duration_seconds":  2,
	}
	for name, limit := range limits {
		if got[name] > limit {
			t.Errorf("%s has %d series, want at most %d", name, got[name], limit)
		}
	}
	if got["pulsego_monitor_checks_total"] != baseline["pulsego_monitor_checks_total"] {
		t.Errorf("monitor_checks_total grew from %d to %d series: unknown kinds must map onto the pre-initialised set",
			baseline["pulsego_monitor_checks_total"], got["pulsego_monitor_checks_total"])
	}

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			for _, lp := range metric.GetLabel() {
				v := lp.GetValue()
				if uuidPattern.MatchString(v) || urlPattern.MatchString(v) {
					t.Errorf("%s label %s=%q holds an ID or URL", f.GetName(), lp.GetName(), v)
				}
			}
		}
	}
}

func TestNormalizeMethod(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want string }{
		{"GET", "GET"}, {"HEAD", "HEAD"}, {"POST", "POST"}, {"PUT", "PUT"},
		{"PATCH", "PATCH"}, {"DELETE", "DELETE"}, {"OPTIONS", "OPTIONS"},
		{"get", "OTHER"}, {"TRACE", "OTHER"}, {"CONNECT", "OTHER"}, {"", "OTHER"}, {"PROPFIND", "OTHER"},
	} {
		if got := telemetry.NormalizeMethod(tt.in); got != tt.want {
			t.Errorf("NormalizeMethod(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeRoute(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want string }{
		{"/v1/monitors/{id}", "/v1/monitors/{id}"},
		{"/healthz", "/healthz"},
		{"", "unmatched"},
		{"v1/monitors", "unmatched"},
		{"http://example.com/", "unmatched"},
		{"/" + strings.Repeat("a", 200), "unmatched"},
	} {
		if got := telemetry.NormalizeRoute(tt.in); got != tt.want {
			t.Errorf("NormalizeRoute(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMetricsLabelValues(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	m.ObserveHTTPRequest("BREW", "", 418, time.Millisecond)
	m.ObserveHTTPRequest(http.MethodGet, "/v1/monitors", 0, time.Millisecond)
	m.ObserveCheck(false, kindPtr(domain.ErrorKindTimeout), time.Second)
	m.ObserveCheck(false, kindPtr("bogus"), time.Second)
	m.ObserveCheck(false, nil, time.Second)
	m.ObserveCheck(true, kindPtr(domain.ErrorKindDNS), time.Second)
	m.StateChanged(domain.StateDegraded, domain.StateDown)
	m.StateChanged(domain.StateUp, domain.StateUp) // self-loop: not a change
	m.StateChanged("bogus", domain.StateUp)
	m.ObserveDBOperation("ListMonitors", nil, time.Millisecond)
	m.ObserveDBOperation("ListMonitors", errors.New("boom"), time.Millisecond)

	checks := []struct {
		desc string
		got  float64
		want float64
	}{
		{"http OTHER/unmatched/418", counterValue(t, m, "pulsego_http_requests_total", "code", "418", "method", "OTHER", "route", "unmatched"), 1},
		{"http code 0 is unknown", counterValue(t, m, "pulsego_http_requests_total", "code", "unknown", "method", "GET", "route", "/v1/monitors"), 1},
		{"check timeout", counterValue(t, m, "pulsego_monitor_checks_total", "error_kind", "timeout", "result", "failure"), 1},
		{"check bogus and nil are unknown", counterValue(t, m, "pulsego_monitor_checks_total", "error_kind", "unknown", "result", "failure"), 2},
		{"success ignores kind", counterValue(t, m, "pulsego_monitor_checks_total", "error_kind", "none", "result", "success"), 1},
		{"degraded to down", counterValue(t, m, "pulsego_monitor_state_total", "from", "degraded", "to", "down"), 1},
		{"invalid from is unknown", counterValue(t, m, "pulsego_monitor_state_total", "from", "unknown", "to", "up"), 1},
		{"up to up never counted", counterValue(t, m, "pulsego_monitor_state_total", "from", "up", "to", "up"), 0},
		{"db ok", histogramCount(t, m, "pulsego_db_operation_duration_seconds", "operation", "ListMonitors", "outcome", "ok"), 1},
		{"db error", histogramCount(t, m, "pulsego_db_operation_duration_seconds", "operation", "ListMonitors", "outcome", "error"), 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.desc, c.got, c.want)
		}
	}
}

func TestMetricsWorkerGaugesAndLag(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	m.AddClaimed(5)
	m.AddClaimed(0)
	m.AddClaimed(-3)
	m.ClaimError()
	m.ActiveChecksInc()
	m.ActiveChecksInc()
	m.ActiveChecksDec()
	m.ObserveSchedulerLag(-5 * time.Second)
	m.ObserveSchedulerLag(3 * time.Second)

	expected := `
# HELP pulsego_worker_claimed_monitors_total Monitors claimed by the worker for checking.
# TYPE pulsego_worker_claimed_monitors_total counter
pulsego_worker_claimed_monitors_total 5
# HELP pulsego_worker_claim_errors_total Failed attempts to claim due monitors.
# TYPE pulsego_worker_claim_errors_total counter
pulsego_worker_claim_errors_total 1
# HELP pulsego_worker_active_checks Checks currently in flight in this worker.
# TYPE pulsego_worker_active_checks gauge
pulsego_worker_active_checks 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected),
		"pulsego_worker_claimed_monitors_total", "pulsego_worker_claim_errors_total", "pulsego_worker_active_checks"); err != nil {
		t.Error(err)
	}

	// The negative lag is clamped to 0, so the sum is exactly 3s and the
	// lowest bucket (0.1s) holds one observation.
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "pulsego_scheduler_lag_seconds" {
			continue
		}
		h := f.GetMetric()[0].GetHistogram()
		if h.GetSampleCount() != 2 || h.GetSampleSum() != 3 {
			t.Errorf("lag count/sum = %d/%v, want 2/3", h.GetSampleCount(), h.GetSampleSum())
		}
		if got := h.GetBucket()[0].GetCumulativeCount(); got != 1 {
			t.Errorf("lag bucket le=0.1 = %d, want 1", got)
		}
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	t.Parallel()

	var m *telemetry.Metrics
	m.ObserveHTTPRequest(http.MethodGet, "/", 200, time.Second)
	m.ObserveCheck(false, nil, time.Second)
	m.StateChanged(domain.StateUp, domain.StateDegraded)
	m.AddClaimed(1)
	m.ClaimError()
	m.ActiveChecksInc()
	m.ActiveChecksDec()
	m.ObserveSchedulerLag(time.Second)
	m.ObserveDBOperation("X", nil, time.Second)
}

// counterValue returns the counter of family name whose labels are exactly
// the given name/value pairs (0 when no such series exists).
func counterValue(t *testing.T, m *telemetry.Metrics, name string, labels ...string) float64 {
	t.Helper()
	counter, _ := seriesValue(t, m, name, labels...)
	return counter
}

// histogramCount is counterValue for the sample count of a histogram.
func histogramCount(t *testing.T, m *telemetry.Metrics, name string, labels ...string) float64 {
	t.Helper()
	_, count := seriesValue(t, m, name, labels...)
	return float64(count)
}

func seriesValue(t *testing.T, m *telemetry.Metrics, name string, labels ...string) (counter float64, histCount uint64) {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	want := make(map[string]string, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, metric := range f.GetMetric() {
			if len(metric.GetLabel()) != len(want) {
				continue
			}
			for _, lp := range metric.GetLabel() {
				if want[lp.GetName()] != lp.GetValue() {
					continue series
				}
			}
			return metric.GetCounter().GetValue(), metric.GetHistogram().GetSampleCount()
		}
	}
	return 0, 0
}
