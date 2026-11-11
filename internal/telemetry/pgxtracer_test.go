package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

func TestOperationName(t *testing.T) {
	t.Parallel()

	tests := []struct{ sql, want string }{
		{"-- name: GetMonitor :one\nSELECT id FROM monitors WHERE id = $1", "GetMonitor"},
		{"  \n-- name: ListMonitors :many\nSELECT 1", "ListMonitors"},
		{"-- name: ClaimDueMonitors :many", "ClaimDueMonitors"},
		{"-- name: Bare\nSELECT 1", "Bare"},
		{"SELECT * FROM monitors WHERE url = 'http://example.com'", "other"},
		{"-- ping", "other"},
		{"", "other"},
		{"-- name: Drop;Table :exec", "other"},
		{"-- name: 1Bad :one", "other"},
		{"-- name: " + strings.Repeat("A", 100) + " :one", "other"},
	}
	for _, tt := range tests {
		if got := telemetry.OperationName(tt.sql); got != tt.want {
			t.Errorf("OperationName(%q) = %q, want %q", tt.sql, got, tt.want)
		}
	}
}

// installRecorder routes the global tracer provider into an in-memory
// recorder for the duration of the test.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	restoreGlobals(t)
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	return rec
}

func TestQueryTracerRecordsMetricAndSpan(t *testing.T) {
	rec := installRecorder(t)
	m := telemetry.NewMetrics()
	tracer := telemetry.NewQueryTracer(m)

	const sql = "-- name: GetMonitor :one\nSELECT id FROM monitors WHERE url = $1"
	secret := "https://example.com/?token=s3cr3t"

	parent, parentSpan := telemetry.Tracer().Start(context.Background(), "parent")
	ctx := tracer.TraceQueryStart(parent, nil, pgx.TraceQueryStartData{SQL: sql, Args: []any{secret}})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	parentSpan.End()

	if got := histogramCount(t, m, "pulsego_db_operation_duration_seconds", "operation", "GetMonitor", "outcome", "ok"); got != 1 {
		t.Errorf("db metric count = %v, want 1", got)
	}

	spans := rec.Ended()
	var db sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "db GetMonitor" {
			db = s
		}
	}
	if db == nil {
		t.Fatalf("no span named %q among %d ended spans", "db GetMonitor", len(spans))
	}
	if db.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", db.SpanKind())
	}
	if db.Parent().SpanID() != parentSpan.SpanContext().SpanID() {
		t.Error("db span is not a child of the caller's span")
	}

	attrs := map[string]string{}
	for _, kv := range db.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["db.system.name"] != "postgresql" || attrs["db.operation.name"] != "GetMonitor" {
		t.Errorf("attributes = %v, want db.system.name=postgresql and db.operation.name=GetMonitor", attrs)
	}
	if len(attrs) != 2 {
		t.Errorf("attributes = %v, want exactly the two db.* attributes", attrs)
	}
	for k, v := range attrs {
		if strings.Contains(v, "SELECT") || strings.Contains(v, "s3cr3t") {
			t.Errorf("attribute %s leaks SQL text or arguments: %q", k, v)
		}
	}
}

func TestQueryTracerRecordsErrorsWithoutMessage(t *testing.T) {
	rec := installRecorder(t)
	m := telemetry.NewMetrics()
	tracer := telemetry.NewQueryTracer(m)

	pgErr := &pgconn.PgError{
		Code:    "23505",
		Message: "duplicate key value violates unique constraint",
		Detail:  "Key (url)=(https://example.com/?token=s3cr3t) already exists.",
	}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "-- name: CreateMonitor :one\nINSERT"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: pgErr})

	ctx = tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("conn reset")})

	if got := histogramCount(t, m, "pulsego_db_operation_duration_seconds", "operation", "CreateMonitor", "outcome", "error"); got != 1 {
		t.Errorf("CreateMonitor error count = %v, want 1", got)
	}
	if got := histogramCount(t, m, "pulsego_db_operation_duration_seconds", "operation", "other", "outcome", "error"); got != 1 {
		t.Errorf("other error count = %v, want 1", got)
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	for _, s := range spans {
		if s.Status().Code != codes.Error {
			t.Errorf("%s status = %v, want error", s.Name(), s.Status().Code)
		}
		if strings.Contains(s.Status().Description, "s3cr3t") || strings.Contains(s.Status().Description, "duplicate") {
			t.Errorf("%s status description leaks the error: %q", s.Name(), s.Status().Description)
		}
		if len(s.Events()) != 0 {
			t.Errorf("%s has %d events, want none (no recorded error message)", s.Name(), len(s.Events()))
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "s3cr3t") {
				t.Errorf("%s attribute %s leaks error detail", s.Name(), kv.Key)
			}
		}
	}
	if spans[0].Name() != "db CreateMonitor" || spans[1].Name() != "db other" {
		t.Errorf("span names = %q, %q", spans[0].Name(), spans[1].Name())
	}
	var sawCode bool
	for _, kv := range spans[0].Attributes() {
		if kv.Key == "db.response.status_code" && kv.Value.AsString() == "23505" {
			sawCode = true
		}
	}
	if !sawCode {
		t.Error("SQLSTATE missing from the failed span")
	}
}

func TestQueryTracerWithoutMetricsOrStart(t *testing.T) {
	installRecorder(t)
	tracer := telemetry.NewQueryTracer(nil)

	// A nil Metrics still traces; an end without a start is ignored.
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "-- name: X :one"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	tracer.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{})
}
