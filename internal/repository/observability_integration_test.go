//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/repository"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// sharedDatabaseURL is the connection string of the migrated shared
// database, for tests that open their own pool through repository.NewPool.
func sharedDatabaseURL(t *testing.T) string {
	t.Helper()
	return sharedPool.Config().ConnString()
}

// TestReadyzWithDatabase runs /readyz on both routers against a real pool,
// first open and then closed (D6, D12).
func TestReadyzWithDatabase(t *testing.T) {
	ctx := context.Background()

	pool, err := repository.NewPool(ctx, sharedDatabaseURL(t), 2, nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	routers := map[string]http.Handler{
		"public": httpapi.NewRouter(httpapi.RouterConfig{
			APIKey: "integration-test-key-0123456789abcdef", Pinger: pool,
		}, slog.New(slog.DiscardHandler)),
		"internal": httpapi.NewInternalRouter(httpapi.InternalConfig{Pinger: pool}, slog.New(slog.DiscardHandler)),
	}

	probe := func(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		return rec
	}

	for name, router := range routers {
		rec := probe(t, router)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /readyz with an open pool: status = %d, want 200 (body %s)", name, rec.Code, rec.Body)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ready" {
			t.Errorf("%s /readyz body = %s, want {\"status\":\"ready\"}", name, rec.Body)
		}
	}

	pool.Close()

	for name, router := range routers {
		rec := probe(t, router)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s /readyz with a closed pool: status = %d, want 503", name, rec.Code)
		}
		var p httpapi.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Type != httpapi.TypeNotReady {
			t.Errorf("%s /readyz body = %s, want a %s problem", name, rec.Body, httpapi.TypeNotReady)
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "closed") {
			t.Errorf("%s /readyz leaks the ping error: %s", name, rec.Body)
		}
	}
}

// TestNewPoolInstallsQueryTracer checks that the tracer passed to NewPool
// observes sqlc queries: metric 10 gets the query name as its operation.
func TestNewPoolInstallsQueryTracer(t *testing.T) {
	ctx := context.Background()
	m := telemetry.NewMetrics()

	pool, err := repository.NewPool(ctx, sharedDatabaseURL(t), 2, telemetry.NewQueryTracer(m))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := sqlcgen.New(pool).ListMonitors(ctx, sqlcgen.ListMonitorsParams{PageLimit: 1}); err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))

	const want = `pulsego_db_operation_duration_seconds_count{operation="ListMonitors",outcome="ok"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics lack %s", want)
	}
}
