package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// TestQAObserveNeverRecordsSecrets sends requests carrying the API key, a
// secret query string, a secret header and a secret body through the public
// router and checks that none of them appears in a span name, span attribute,
// span event or metric line (CLAUDE.md rule 10, Sprint 5 D3/D4).
func TestQAObserveNeverRecordsSecrets(t *testing.T) { //nolint:paralleltest // mutates the global tracer provider
	rec := installSpanRecorder(t)
	m := telemetry.NewMetrics()
	router := httpapi.NewRouter(httpapi.RouterConfig{
		APIKey: testAPIKey, Monitors: &stubService{}, Pinger: errPinger{}, Metrics: m,
	}, discardLogger())

	const (
		querySecret  = "qsecret-7c1f"
		headerSecret = "hsecret-9d2e"
		bodySecret   = "bsecret-4a8b"
	)
	reqs := []*http.Request{
		authedRequest(t, http.MethodGet, "/v1/monitors?cursor="+querySecret, nil),
		authedRequest(t, http.MethodPost, "/v1/monitors?x="+querySecret,
			`{"name":"`+bodySecret+`","url":"https://example.com/?k=`+bodySecret+`"}`),
		authedRequest(t, http.MethodGet, "/v1/monitors/"+testMonitorID+"?t="+querySecret, nil),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/nope/"+querySecret, nil),
	}
	wrongKey := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors", nil)
	wrongKey.Header.Set("Authorization", "Bearer "+headerSecret)
	reqs = append(reqs, wrongKey)
	for _, r := range reqs {
		r.Header.Set("X-Custom", headerSecret)
		router.ServeHTTP(httptest.NewRecorder(), r)
	}

	secrets := []string{testAPIKey, querySecret, headerSecret, bodySecret, testMonitorID}
	check := func(where, s string) {
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("%s holds %q: %s", where, secret, s)
			}
		}
	}

	spans := rec.Ended()
	if len(spans) < len(reqs) {
		t.Fatalf("recorded %d spans, want at least %d", len(spans), len(reqs))
	}
	for _, s := range spans {
		check("span name", s.Name())
		for _, kv := range s.Attributes() {
			check("span attribute "+string(kv.Key), kv.Value.String())
		}
		for _, ev := range s.Events() {
			check("span event", ev.Name)
			for _, kv := range ev.Attributes {
				check("span event attribute "+string(kv.Key), kv.Value.String())
			}
		}
		check("span status", s.Status().Description)
	}
	for line := range strings.Lines(scrape(t, m)) {
		if strings.HasPrefix(line, "pulsego_") {
			check("metric line", line)
		}
	}
}
