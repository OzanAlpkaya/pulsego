// Package telemetry wires the observability stack: structured logging,
// Prometheus metrics on a private registry, OpenTelemetry tracing over
// OTLP/HTTP, and the pgx query tracer that feeds both. Everything here must
// keep degrading gracefully when an exporter is disabled or unreachable.
package telemetry

import (
	"io"
	"log/slog"
)

// NewLogger builds the JSON logger used by both binaries. Every record
// carries the service name and environment so that logs from the API and the
// worker can be told apart once they are shipped somewhere central.
func NewLogger(w io.Writer, level slog.Level, service, environment string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})

	return slog.New(handler).With(
		slog.String("service", service),
		slog.String("env", environment),
	)
}
