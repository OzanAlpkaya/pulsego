package httpapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

const testMonitorID = "11111111-2222-3333-4444-555555555555"

// scrape returns the text exposition of m.
func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestObserveRecordsRouteTemplates drives the public router with raw paths
// that carry IDs and scanner noise, and checks that the request metrics only
// ever hold route templates (D3 cardinality rules).
func TestObserveRecordsRouteTemplates(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	router := httpapi.NewRouter(httpapi.RouterConfig{
		APIKey: testAPIKey, Monitors: &stubService{}, Pinger: errPinger{}, Metrics: m,
	}, discardLogger())

	requests := []*http.Request{
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/readyz", nil),
		authedRequest(t, http.MethodGet, "/v1/monitors/"+testMonitorID, nil),
		authedRequest(t, http.MethodGet, "/v1/monitors?limit=5", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors/"+testMonitorID, nil), // 401
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/wp-admin/"+testMonitorID+".php", nil),
		httptest.NewRequestWithContext(t.Context(), "PROPFIND", "/healthz", nil),
		// Dashboard files share one template whichever file is asked for.
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/app.js", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/style.css", nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/static/"+testMonitorID+".js", nil),
	}
	for _, req := range requests {
		router.ServeHTTP(httptest.NewRecorder(), req)
	}

	body := scrape(t, m)
	for _, want := range []string{
		`pulsego_http_requests_total{code="200",method="GET",route="/healthz"} 1`,
		`pulsego_http_requests_total{code="200",method="HEAD",route="/readyz"} 1`,
		`pulsego_http_requests_total{code="200",method="GET",route="/v1/monitors/{monitorID}"} 1`,
		`pulsego_http_requests_total{code="200",method="GET",route="/v1/monitors"} 1`,
		// Auth rejects before chi matches the inner route, so the pattern is
		// the /v1 mount: still a template, and one series for every 401.
		`pulsego_http_requests_total{code="401",method="GET",route="/v1/*"} 1`,
		`pulsego_http_requests_total{code="404",method="GET",route="unmatched"} 1`,
		`pulsego_http_requests_total{code="200",method="GET",route="/"} 1`,
		`pulsego_http_requests_total{code="200",method="GET",route="/static/*"} 2`,
		`pulsego_http_requests_total{code="404",method="GET",route="/static/*"} 1`,
		`pulsego_http_requests_total{code="405",method="OTHER",route="unmatched"} 1`,
		`pulsego_http_request_duration_seconds_count{method="GET",route="/v1/monitors/{monitorID}"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	uuidPattern := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, "pulsego_http_") && (uuidPattern.MatchString(line) || strings.Contains(line, "?")) {
			t.Errorf("metric line holds a raw path: %s", line)
		}
	}
}

// installSpanRecorder routes the global tracer provider and propagator into
// an in-memory recorder for the test. It mutates globals, so callers must not
// run in parallel with each other.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	prevProvider, prevPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})

	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return rec
}

// spanNamed returns the one ended span called name.
func spanNamed(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		names := make([]string, 0, len(rec.Ended()))
		for _, s := range rec.Ended() {
			names = append(names, s.Name())
		}
		t.Fatalf("found %d spans named %q, want 1 (ended: %v)", len(found), name, names)
	}
	return found[0]
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value)
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestObserveStartsServerSpanWithPropagatedParent(t *testing.T) { //nolint:paralleltest // mutates the global tracer provider
	rec := installSpanRecorder(t)

	router := httpapi.NewRouter(httpapi.RouterConfig{APIKey: testAPIKey, Monitors: &stubService{}}, discardLogger())

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := authedRequest(t, http.MethodGet, "/v1/monitors/"+testMonitorID+"?token=s3cr3t", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	router.ServeHTTP(httptest.NewRecorder(), req)

	span := spanNamed(t, rec, "GET /v1/monitors/{monitorID}")

	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", span.SpanKind())
	}
	if got := span.SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace id = %s, want the propagated %s", got, traceID)
	}
	if !span.Parent().IsRemote() {
		t.Error("parent is not the remote span from traceparent")
	}

	a := attrs(span)
	if got := a["http.request.method"].AsString(); got != http.MethodGet {
		t.Errorf("http.request.method = %q, want GET", got)
	}
	if got := a["http.route"].AsString(); got != "/v1/monitors/{monitorID}" {
		t.Errorf("http.route = %q, want the template", got)
	}
	if got := a["http.response.status_code"].AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d, want 200", got)
	}
	if span.Status().Code == codes.Error {
		t.Errorf("a 200 span has error status: %+v", span.Status())
	}

	// Nothing from the raw request may reach the span.
	for k, v := range a {
		if s := v.String(); strings.Contains(s, testMonitorID) || strings.Contains(s, "s3cr3t") || strings.Contains(s, testAPIKey) {
			t.Errorf("attribute %s leaks request data: %s", k, s)
		}
	}
}

func TestObserveMarksServerErrors(t *testing.T) { //nolint:paralleltest // mutates the global tracer provider
	rec := installSpanRecorder(t)

	m := telemetry.NewMetrics()
	router := httpapi.NewRouter(httpapi.RouterConfig{
		APIKey: testAPIKey, Monitors: &stubService{err: errors.New("database exploded")}, Metrics: m,
	}, discardLogger())
	router.ServeHTTP(httptest.NewRecorder(), authedRequest(t, http.MethodGet, "/v1/monitors/"+testMonitorID, nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nope", nil))

	span := spanNamed(t, rec, "GET /v1/monitors/{monitorID}")
	if span.Status().Code != codes.Error {
		t.Errorf("5xx span status = %v, want error", span.Status().Code)
	}
	if got := attrs(span)["http.response.status_code"].AsInt64(); got != http.StatusInternalServerError {
		t.Errorf("http.response.status_code = %d, want 500", got)
	}

	unmatched := spanNamed(t, rec, "GET unmatched")
	if unmatched.Status().Code == codes.Error {
		t.Error("a 404 must not mark the span as an error")
	}

	if !strings.Contains(scrape(t, m), `pulsego_http_requests_total{code="500",method="GET",route="/v1/monitors/{monitorID}"} 1`) {
		t.Error("the 500 was not counted")
	}
}
