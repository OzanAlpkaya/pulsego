//go:build integration

package repository_test

import (
	"strings"
	"testing"
)

// TestMonitorChecksConstraints checks that every bound internal/domain/check.go
// declares for a stored check, and every outcome-consistency rule, is also
// enforced by the database, so a bad result cannot land even if the checker or
// the repository is wrong.
func TestMonitorChecksConstraints(t *testing.T) {
	url := startPostgres(t)
	sqlDB := openSQL(t, url)
	migrateUp(t, sqlDB)

	var monitorID string
	if err := sqlDB.QueryRowContext(t.Context(), `
		INSERT INTO monitors (name, url, interval_seconds, timeout_ms,
		                      expected_status_min, expected_status_max, failure_threshold)
		VALUES ('example', 'https://example.com', 60, 5000, 200, 399, 3)
		RETURNING id`).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}

	const insert = `
		INSERT INTO monitor_checks (monitor_id, success, status_code, latency_ms,
		                            error_kind, error_message)
		VALUES ($1, $2, $3, $4, $5, $6)`

	type row struct {
		success      bool
		statusCode   *int
		latencyMS    int
		errorKind    *string
		errorMessage *string
	}

	ptr := func(v int) *int { return &v }
	str := func(s string) *string { return &s }

	success := row{success: true, statusCode: ptr(200), latencyMS: 42}
	failure := row{success: false, latencyMS: 42, errorKind: str("connection"), errorMessage: str("connection refused")}

	exec := func(t *testing.T, r row) error {
		t.Helper()
		_, err := sqlDB.ExecContext(t.Context(), insert, monitorID, r.success, r.statusCode, r.latencyMS, r.errorKind, r.errorMessage)
		return err
	}

	// The valid rows must be accepted, otherwise every rejection below is
	// meaningless. The failure row carries a message of exactly the maximum
	// length, so the length bound is proven inclusive.
	longest := failure
	longest.errorMessage = str(strings.Repeat("x", 512))
	unexpected := row{success: false, statusCode: ptr(503), latencyMS: 42, errorKind: str("unexpected_status"), errorMessage: str("status 503 outside expected range 200-399")}
	for name, r := range map[string]row{"success": success, "failure": failure, "longest message": longest, "unexpected status": unexpected} {
		if err := exec(t, r); err != nil {
			t.Fatalf("valid %s check was rejected: %v", name, err)
		}
	}

	tests := []struct {
		name       string
		base       row
		mutate     func(*row)
		constraint string
	}{
		{name: "status code too small", base: success, mutate: func(r *row) { r.statusCode = ptr(99) }, constraint: "monitor_checks_status_code_range"},
		{name: "status code too large", base: success, mutate: func(r *row) { r.statusCode = ptr(600) }, constraint: "monitor_checks_status_code_range"},
		{name: "negative latency", base: success, mutate: func(r *row) { r.latencyMS = -1 }, constraint: "monitor_checks_latency_non_negative"},
		{name: "unknown error kind", base: failure, mutate: func(r *row) { r.errorKind = str("http") }, constraint: "monitor_checks_error_kind_valid"},
		{name: "error message too long", base: failure, mutate: func(r *row) { r.errorMessage = str(strings.Repeat("x", 513)) }, constraint: "monitor_checks_error_message_length"},
		{name: "success with error kind", base: success, mutate: func(r *row) { r.errorKind = str("timeout") }, constraint: "monitor_checks_outcome_consistent"},
		{name: "failure without error kind", base: failure, mutate: func(r *row) { r.errorKind, r.errorMessage = nil, nil }, constraint: "monitor_checks_outcome_consistent"},
		{name: "success with error message", base: success, mutate: func(r *row) { r.errorMessage = str("unexpected") }, constraint: "monitor_checks_message_only_on_failure"},
		{name: "success without status code", base: success, mutate: func(r *row) { r.statusCode = nil }, constraint: "monitor_checks_success_has_status"},
		{name: "unexpected status without status code", base: unexpected, mutate: func(r *row) { r.statusCode = nil }, constraint: "monitor_checks_unexpected_status_has_status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.base
			tt.mutate(&r)

			err := exec(t, r)
			if err == nil {
				t.Fatalf("insert succeeded, want a violation of %s", tt.constraint)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("error = %v, want a violation of %s", err, tt.constraint)
			}
		})
	}
}
