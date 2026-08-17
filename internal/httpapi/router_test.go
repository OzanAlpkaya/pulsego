package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	routers := map[string]http.Handler{
		"public":   newTestRouter(t, &stubService{}),
		"internal": httpapi.NewInternalRouter(),
	}

	for name, router := range routers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))

			if got, want := rec.Code, http.StatusOK; got != want {
				t.Errorf("status = %d, want %d", got, want)
			}
			if got, want := rec.Header().Get("Content-Type"), "application/json"; got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %v (body: %q)", err, rec.Body.String())
			}
			if got, want := body["status"], "ok"; got != want {
				t.Errorf("status field = %q, want %q", got, want)
			}
		})
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newTestRouter(t, &stubService{}).
		ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/does-not-exist", nil))

	if got, want := rec.Code, http.StatusNotFound; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
}

// TestRequestLoggerUsesRoutePattern guards the cardinality property the
// Prometheus labels will depend on in Sprint 5: the logged route must be the
// template, and unmatched paths must all collapse to one value.
func TestRequestLoggerUsesRoutePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		wantRoute string
	}{
		{name: "matched route", path: "/healthz", wantRoute: "/healthz"},
		{name: "unmatched route", path: "/wp-admin/setup.php", wantRoute: "unmatched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))

			rec := httptest.NewRecorder()
			httpapi.NewRouter(httpapi.RouterConfig{APIKey: testAPIKey, Monitors: &stubService{}}, log).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("log output is not valid JSON: %v (output: %q)", err, buf.String())
			}
			if got, want := record["route"], tt.wantRoute; got != want {
				t.Errorf("logged route = %v, want %v", got, want)
			}
			if _, ok := record["duration"]; !ok {
				t.Errorf("log record is missing a duration field: %v", record)
			}
		})
	}
}
