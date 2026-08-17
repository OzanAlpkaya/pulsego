// Package telemetry wires the observability stack. For now that is only
// structured logging; Prometheus metrics and OpenTelemetry tracing join it in
// Sprint 5, and everything here must keep degrading gracefully when an
// exporter is disabled or unreachable.
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
