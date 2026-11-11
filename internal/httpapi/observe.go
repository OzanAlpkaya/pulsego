package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// HTTPMetrics is what Observe needs from the metrics registry.
// *telemetry.Metrics satisfies it, including as a nil pointer.
type HTTPMetrics interface {
	ObserveHTTPRequest(method, route string, code int, d time.Duration)
}

// Observe records the request metrics (D3, metrics 1–2) and a server span
// (D4) for every request on the public router.
//
// The route label and the span name are read from chi after the handler ran:
// only then has the router matched the full pattern, including mounted
// sub-routers. Unmatched requests collapse into "unmatched". Neither the
// metrics nor the span carry the raw path, query string, headers or body.
//
// A nil m records spans only.
func Observe(m HTTPMetrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			method := telemetry.NormalizeMethod(r.Method)

			// The parent comes from the W3C traceparent header when the caller
			// sent one; SetupTracing installs the propagator, and the default
			// global one is a no-op.
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := telemetry.Tracer().Start(ctx, method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(attribute.String("http.request.method", method)),
			)
			defer span.End()

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r.WithContext(ctx))

			// chi's route context is shared by pointer, so the original request
			// sees the pattern the handler chain matched.
			route := telemetry.NormalizeRoute(routePattern(r))
			code := ww.Status()
			if code == 0 {
				// Nothing was written: net/http sends an implicit 200.
				code = http.StatusOK
			}

			span.SetName(method + " " + route)
			span.SetAttributes(
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", code),
			)
			if code >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, strconv.Itoa(code))
			}

			if m != nil {
				m.ObserveHTTPRequest(method, route, code, time.Since(started))
			}
		})
	}
}
