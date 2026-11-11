// Package httpapi holds the HTTP transport layer: routing, middleware, and
// request/response encoding. Handlers translate between HTTP and the domain
// and keep business rules out of this package.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/OzanAlpkaya/pulsego/web"
)

// RouterConfig carries what the public router needs to be built.
type RouterConfig struct {
	APIKey   string
	Monitors MonitorService
	// Pinger backs /readyz. Nil makes /readyz always report not ready.
	Pinger Pinger
	// Metrics receives the per-request metrics. Nil records spans only.
	Metrics HTTPMetrics
}

// NewRouter builds the public API router served on HTTP_ADDR.
//
// Health endpoints sit outside the authenticated group: a probe must not need
// a credential, and /healthz is what Docker and load balancers call.
func NewRouter(cfg RouterConfig, log *slog.Logger) http.Handler {
	r := chi.NewRouter()

	// No RealIP middleware: it trusts client-supplied forwarding headers, and
	// nothing here needs the caller's address badly enough to accept that.
	r.Use(middleware.RequestID)
	// Observe sits outside Recoverer so a recovered panic is still recorded,
	// as the 500 Recoverer writes.
	r.Use(Observe(cfg.Metrics))
	r.Use(middleware.Recoverer)
	r.Use(RequestLogger(log))

	// HEAD as well as GET: container and load-balancer probes commonly use it.
	r.Get("/healthz", Healthz)
	r.Head("/healthz", Healthz)
	r.Get("/readyz", Readyz(cfg.Pinger, log))
	r.Head("/readyz", Readyz(cfg.Pinger, log))

	// The dashboard is public because it carries no data: the page asks for
	// the API key and calls /v1 from the browser. Both patterns are fixed, so
	// they keep the route label bounded.
	static := web.Static()
	r.Group(func(dash chi.Router) {
		dash.Use(dashboardHeaders)
		dash.Get("/", dashboardIndex(static))
		dash.Get("/static/*", dashboardStatic(static))
	})

	r.Route("/v1", func(v1 chi.Router) {
		v1.Use(APIKeyAuth(cfg.APIKey, log))

		v1.Mount("/monitors", NewMonitorHandler(cfg.Monitors, log).Routes())
	})

	return r
}

// InternalConfig carries what the internal router needs to be built.
type InternalConfig struct {
	// Pinger backs /readyz. Nil makes /readyz always report not ready.
	Pinger Pinger
	// Metrics serves /metrics, normally telemetry.Metrics.Handler(). When nil
	// the route is not registered.
	Metrics http.Handler
}

// NewInternalRouter builds the operator-facing router served on
// INTERNAL_HTTP_ADDR. It is not exposed publicly, which is why /metrics lives
// here rather than next to the API routes. Its requests are neither logged
// nor observed: a scrape every few seconds would drown out the application's
// own records and series.
func NewInternalRouter(cfg InternalConfig, log *slog.Logger) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)

	r.Get("/healthz", Healthz)
	r.Head("/healthz", Healthz)
	r.Get("/readyz", Readyz(cfg.Pinger, log))
	r.Head("/readyz", Readyz(cfg.Pinger, log))

	if cfg.Metrics != nil {
		r.Method(http.MethodGet, "/metrics", cfg.Metrics)
	}

	return r
}

// Healthz reports process liveness. It deliberately performs no database
// query: an unreachable database must not make a healthy process look dead
// and get restarted. Readiness (/readyz) is what checks dependencies.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
