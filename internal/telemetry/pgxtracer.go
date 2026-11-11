package telemetry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// sqlcNamePrefix starts every query sqlc generates: "-- name: GetMonitor :one".
const sqlcNamePrefix = "-- name: "

// QueryTracer is the single pgx tracer of the pool (D5). For every query it
// records metric 10 and a "db <operation>" client span. Neither carries the
// SQL text, the arguments or a driver error message, any of which could hold
// monitor URLs or other user data.
type QueryTracer struct {
	metrics *Metrics
}

var _ pgx.QueryTracer = (*QueryTracer)(nil)

// NewQueryTracer returns a tracer that records into m (nil disables the
// metric, spans are still produced). Spans use the global tracer provider at
// query time, so the tracer may be created before SetupTracing runs.
func NewQueryTracer(m *Metrics) pgx.QueryTracer {
	return &QueryTracer{metrics: m}
}

type queryTraceKey struct{}

type queryTrace struct {
	op    string
	start time.Time
	span  trace.Span
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	op := OperationName(data.SQL)
	ctx, span := Tracer().Start(ctx, "db "+op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", op),
		),
	)
	return context.WithValue(ctx, queryTraceKey{}, &queryTrace{op: op, start: time.Now(), span: span})
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	qt, ok := ctx.Value(queryTraceKey{}).(*queryTrace)
	if !ok {
		return
	}
	t.metrics.ObserveDBOperation(qt.op, data.Err, time.Since(qt.start))

	if data.Err != nil {
		// Only the SQLSTATE goes on the span: a PostgreSQL error message or
		// detail can quote the offending values.
		var pgErr *pgconn.PgError
		if errors.As(data.Err, &pgErr) {
			qt.span.SetAttributes(attribute.String("db.response.status_code", pgErr.Code))
		}
		qt.span.SetStatus(codes.Error, "query failed")
	}
	qt.span.End()
}

// OperationName extracts the sqlc query name from the "-- name: X" prefix of
// sql, or returns "other" for any statement sqlc did not generate.
func OperationName(sql string) string {
	rest, ok := strings.CutPrefix(strings.TrimLeft(sql, " \t\r\n"), sqlcNamePrefix)
	if !ok {
		return opOther
	}
	name, _, _ := strings.Cut(rest, " ")
	name, _, _ = strings.Cut(name, "\n")
	return normalizeOperation(strings.TrimSpace(name))
}
