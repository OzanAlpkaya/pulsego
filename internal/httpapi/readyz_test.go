package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// errPinger is a stateless Pinger, safe to share between parallel subtests.
type errPinger struct{ err error }

func (p errPinger) Ping(context.Context) error { return p.err }

// fakePinger returns err and remembers the deadline it was given. Use one per
// test: it is not safe for concurrent use.
type fakePinger struct {
	err         error
	gotDeadline time.Duration
	hadDeadline bool
}

func (p *fakePinger) Ping(ctx context.Context) error {
	if d, ok := ctx.Deadline(); ok {
		p.hadDeadline, p.gotDeadline = true, time.Until(d)
	}
	return p.err
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// readyzRouters builds both routers that serve /readyz around one pinger.
func readyzRouters(p httpapi.Pinger) map[string]http.Handler {
	return map[string]http.Handler{
		"public": httpapi.NewRouter(httpapi.RouterConfig{
			APIKey: testAPIKey, Monitors: &stubService{}, Pinger: p,
		}, discardLogger()),
		"internal": httpapi.NewInternalRouter(httpapi.InternalConfig{Pinger: p}, discardLogger()),
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	// The secret-looking error must never reach the response body.
	const secret = "password authentication failed for user pulsego at db.internal"

	tests := []struct {
		name       string
		pinger     httpapi.Pinger
		wantStatus int
	}{
		{name: "database reachable", pinger: errPinger{}, wantStatus: http.StatusOK},
		{name: "database unreachable", pinger: errPinger{err: errors.New(secret)}, wantStatus: http.StatusServiceUnavailable},
		{name: "no pinger configured", pinger: nil, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		for routerName, router := range readyzRouters(tt.pinger) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(tt.name+"/"+routerName+"/"+method, func(t *testing.T) {
					t.Parallel()

					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/readyz", nil))

					if rec.Code != tt.wantStatus {
						t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
					}
					if method == http.MethodHead {
						return
					}
					if strings.Contains(rec.Body.String(), secret) {
						t.Errorf("body leaks the ping error: %s", rec.Body.String())
					}

					if tt.wantStatus == http.StatusOK {
						var body map[string]string
						if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
							t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
						}
						if body["status"] != "ready" {
							t.Errorf("status field = %q, want ready", body["status"])
						}
						return
					}

					if got := rec.Header().Get("Content-Type"); got != httpapi.ContentTypeProblem {
						t.Errorf("Content-Type = %q, want %q", got, httpapi.ContentTypeProblem)
					}
					var p httpapi.Problem
					if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
						t.Fatalf("body is not a problem document: %v (%q)", err, rec.Body.String())
					}
					if p.Type != httpapi.TypeNotReady || p.Status != http.StatusServiceUnavailable {
						t.Errorf("problem = %+v, want type %s and status 503", p, httpapi.TypeNotReady)
					}
				})
			}
		}
	}
}

func TestReadyzBoundsThePing(t *testing.T) {
	t.Parallel()

	p := &fakePinger{}
	rec := httptest.NewRecorder()
	httpapi.Readyz(p, discardLogger()).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if !p.hadDeadline {
		t.Fatal("ping context has no deadline")
	}
	if p.gotDeadline <= 0 || p.gotDeadline > 2*time.Second {
		t.Errorf("ping deadline = %v from now, want within 2s", p.gotDeadline)
	}
}

func TestInternalRouterServesMetrics(t *testing.T) {
	t.Parallel()

	m := telemetry.NewMetrics()
	internal := httpapi.NewInternalRouter(httpapi.InternalConfig{Pinger: &fakePinger{}, Metrics: m.Handler()}, discardLogger())

	rec := httptest.NewRecorder()
	internal.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, name := range []string{"pulsego_monitor_checks_total", "pulsego_monitor_state_total", "go_goroutines"} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Errorf("/metrics does not expose %s", name)
		}
	}

	// The internal router is not instrumented: its own scrapes add no series.
	rec = httptest.NewRecorder()
	internal.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), `route="/metrics"`) {
		t.Error("internal requests were recorded as HTTP request metrics")
	}
}

func TestPublicRouterDoesNotServeMetrics(t *testing.T) {
	t.Parallel()

	public := httpapi.NewRouter(httpapi.RouterConfig{
		APIKey: testAPIKey, Monitors: &stubService{}, Metrics: telemetry.NewMetrics(),
	}, discardLogger())

	rec := httptest.NewRecorder()
	public.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("public /metrics status = %d, want 404", rec.Code)
	}
}
