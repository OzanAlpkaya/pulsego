package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracerName identifies the instrumentation scope of every span PulseGo
// creates itself.
const tracerName = "github.com/OzanAlpkaya/pulsego"

// tracesPath is the OTLP/HTTP signal path appended to the configured base
// endpoint, as the OpenTelemetry specification prescribes for
// OTEL_EXPORTER_OTLP_ENDPOINT.
const tracesPath = "/v1/traces"

// TracingConfig selects whether and where spans are exported.
type TracingConfig struct {
	Enabled bool
	// Endpoint is the OTLP/HTTP base URL of the collector, for example
	// http://otel-collector:4318. The signal path /v1/traces is appended.
	Endpoint    string
	ServiceName string
	Environment string
}

// Tracer returns the PulseGo tracer from the global provider. Looking it up at
// call time means spans follow whatever SetupTracing installed, including the
// no-op provider when tracing is disabled.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// TracerFrom returns the PulseGo tracer from tp, or from the global provider
// (as Tracer does) when tp is nil. Components take an optional provider so
// tests can record spans without touching the global one.
func TracerFrom(tp trace.TracerProvider) trace.Tracer {
	if tp == nil {
		return Tracer()
	}
	return tp.Tracer(tracerName)
}

// SetupTracing installs the global tracer provider and propagator and returns
// a shutdown function that flushes pending spans; the caller bounds it with a
// context deadline (SHUTDOWN_TIMEOUT) and logs, never fails on, its error.
//
// Disabled, it installs a no-op provider and performs no network I/O at all.
// Enabled, it exports OTLP/HTTP protobuf through a batch processor. Building
// the exporter never dials, and exports run in the background, so an
// unreachable collector can neither block startup nor slow down requests;
// export failures are logged at warn level.
func SetupTracing(ctx context.Context, cfg TracingConfig, log *slog.Logger) (func(context.Context) error, error) {
	noopShutdown := func(context.Context) error { return nil }

	if !cfg.Enabled {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return noopShutdown, nil
	}

	endpoint, err := TracesEndpointURL(cfg.Endpoint)
	if err != nil {
		return noopShutdown, err
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry error", slog.String("error", err.Error()))
	}))

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return noopShutdown, fmt.Errorf("create otlp trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("deployment.environment", cfg.Environment),
	))
	if err != nil {
		// Merge only fails on conflicting schema URLs; fall back to our own
		// attributes rather than refusing to start.
		res = resource.NewSchemaless(
			attribute.String("service.name", cfg.ServiceName),
			attribute.String("deployment.environment", cfg.Environment),
		)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider.Shutdown, nil
}

// TracesEndpointURL validates an OTLP/HTTP base URL and returns the full
// traces URL. A base path is kept and /v1/traces appended to it, so both
// http://collector:4318 and http://gateway/otlp work.
//
// otlptracehttp.WithEndpointURL posts to the URL's path verbatim, and to "/"
// when the path is empty, so the signal path must be added here.
func TracesEndpointURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("otlp endpoint is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("otlp endpoint must use the http:// or https:// scheme")
	}
	if u.Hostname() == "" {
		return "", errors.New("otlp endpoint must include a host")
	}
	u.Path = path.Join("/", u.Path, tracesPath)
	u.RawPath = ""
	return u.String(), nil
}
