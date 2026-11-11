//go:build integration

package repository_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

// TestQueryTracerRecordsOperationNames runs real sqlc queries through a pool
// carrying the telemetry query tracer and checks that metric 10 is labelled
// with the sqlc query names (D5, D12).
func TestQueryTracerRecordsOperationNames(t *testing.T) {
	ctx := context.Background()
	m := telemetry.NewMetrics()

	cfg := sharedPool.Config().Copy()
	cfg.ConnConfig.Tracer = telemetry.NewQueryTracer(m)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	defer pool.Close()

	q := sqlcgen.New(pool)
	if _, err := q.ListMonitors(ctx, sqlcgen.ListMonitorsParams{PageLimit: 1}); err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if _, err := q.GetMonitor(ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetMonitor of a missing id: err = %v, want pgx.ErrNoRows", err)
	}
	if _, err := pool.Exec(ctx, "SELECT * FROM no_such_table"); err == nil {
		t.Fatal("query on a missing table succeeded")
	}

	want := map[[2]string]bool{
		{"ListMonitors", "ok"}: false,
		{"GetMonitor", "ok"}:   false, // no rows is not a database error
		{"other", "error"}:     false,
	}
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "pulsego_db_operation_duration_seconds" {
			continue
		}
		for _, metric := range f.GetMetric() {
			var op, outcome string
			for _, lp := range metric.GetLabel() {
				switch lp.GetName() {
				case "operation":
					op = lp.GetValue()
				case "outcome":
					outcome = lp.GetValue()
				}
			}
			key := [2]string{op, outcome}
			if _, ok := want[key]; ok && metric.GetHistogram().GetSampleCount() > 0 {
				want[key] = true
			}
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("no observation for operation=%s outcome=%s", key[0], key[1])
		}
	}
}
