// Package httpapi holds the HTTP transport layer: routing, middleware, and
// request/response encoding. Handlers translate between HTTP and the domain
// and keep business rules out of this package.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds the public API router served on HTTP_ADDR.
//
// Sprint 1 only establishes the middleware stack and the liveness probe. API
// key authentication, request IDs, and problem+json errors land in Sprint 2,
// and /readyz joins the internal router in Sprint 5.
func NewRouter(log *slog.Logger) http.Handler {
	r := chi.NewRouter()

	// No RealIP middleware: it trusts client-supplied forwarding headers, and
	// nothing here needs the caller's address badly enough to accept that.
	r.Use(middleware.Recoverer)
	r.Use(RequestLogger(log))

	// HEAD as well as GET: container and load-balancer probes commonly use it.
	r.Get("/healthz", Healthz)
	r.Head("/healthz", Healthz)

	return r
}

// NewInternalRouter builds the operator-facing router served on
// INTERNAL_HTTP_ADDR. It is not exposed publicly, which is why /metrics will
// live here rather than next to the API routes. Its requests are not logged:
// a scrape every few seconds would drown out the application's own records.
func NewInternalRouter() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)

	// HEAD as well as GET: container and load-balancer probes commonly use it.
	r.Get("/healthz", Healthz)
	r.Head("/healthz", Healthz)

	return r
}

// Healthz reports process liveness. It deliberately performs no database
// query: an unreachable database must not make a healthy process look dead
// and get restarted. Readiness (Sprint 5) is what checks dependencies.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
