package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
)

// MonitorStore implements monitor.Store on top of the generated queries.
type MonitorStore struct {
	q *sqlcgen.Queries
}

// NewMonitorStore builds a store over a connection pool.
func NewMonitorStore(pool *pgxpool.Pool) *MonitorStore {
	return &MonitorStore{q: sqlcgen.New(pool)}
}

// Create inserts a monitor and returns the stored row.
func (s *MonitorStore) Create(ctx context.Context, p monitor.CreateParams) (domain.Monitor, error) {
	row, err := s.q.CreateMonitor(ctx, sqlcgen.CreateMonitorParams{
		Name:              p.Name,
		Url:               p.URL,
		IntervalSeconds:   p.IntervalSeconds,
		TimeoutMs:         p.TimeoutMS,
		ExpectedStatusMin: p.ExpectedStatusMin,
		ExpectedStatusMax: p.ExpectedStatusMax,
		FailureThreshold:  p.FailureThreshold,
		Enabled:           p.Enabled,
	})
	if err != nil {
		return domain.Monitor{}, translateError(err, "create monitor")
	}
	return toDomain(row), nil
}

// Get returns a monitor by id, or domain.ErrNotFound if it is absent or
// soft-deleted.
func (s *MonitorStore) Get(ctx context.Context, id uuid.UUID) (domain.Monitor, error) {
	row, err := s.q.GetMonitor(ctx, id)
	if err != nil {
		return domain.Monitor{}, translateError(err, "get monitor")
	}
	return toDomain(row), nil
}

// List returns one page of monitors, newest first, excluding soft-deleted rows.
func (s *MonitorStore) List(ctx context.Context, p monitor.ListParams) ([]domain.Monitor, error) {
	params := sqlcgen.ListMonitorsParams{PageLimit: p.Limit}
	if p.Cursor != nil {
		params.CursorCreatedAt = &p.Cursor.CreatedAt
		params.CursorID = p.Cursor.ID
	}

	rows, err := s.q.ListMonitors(ctx, params)
	if err != nil {
		return nil, translateError(err, "list monitors")
	}

	monitors := make([]domain.Monitor, 0, len(rows))
	for _, row := range rows {
		monitors = append(monitors, toDomain(row))
	}
	return monitors, nil
}

// Update applies the set fields of p and returns the updated row.
func (s *MonitorStore) Update(ctx context.Context, p monitor.UpdateParams) (domain.Monitor, error) {
	row, err := s.q.UpdateMonitor(ctx, sqlcgen.UpdateMonitorParams{
		ID:                p.ID,
		Name:              p.Name,
		Url:               p.URL,
		IntervalSeconds:   p.IntervalSeconds,
		TimeoutMs:         p.TimeoutMS,
		ExpectedStatusMin: p.ExpectedStatusMin,
		ExpectedStatusMax: p.ExpectedStatusMax,
		FailureThreshold:  p.FailureThreshold,
		Enabled:           p.Enabled,
	})
	if err != nil {
		return domain.Monitor{}, translateError(err, "update monitor")
	}
	return toDomain(row), nil
}

// SoftDelete marks a monitor deleted. Deleting an already-deleted monitor
// returns domain.ErrNotFound.
func (s *MonitorStore) SoftDelete(ctx context.Context, id uuid.UUID) error {
	if _, err := s.q.SoftDeleteMonitor(ctx, id); err != nil {
		return translateError(err, "soft delete monitor")
	}
	return nil
}

// toDomain maps a generated row onto the domain type. The two structs are
// deliberately separate: the generated one tracks the table, the domain one
// tracks what the rest of the application needs.
func toDomain(row sqlcgen.Monitor) domain.Monitor {
	return domain.Monitor{
		ID:                  row.ID,
		Name:                row.Name,
		URL:                 row.Url,
		IntervalSeconds:     row.IntervalSeconds,
		TimeoutMS:           row.TimeoutMs,
		ExpectedStatusMin:   row.ExpectedStatusMin,
		ExpectedStatusMax:   row.ExpectedStatusMax,
		FailureThreshold:    row.FailureThreshold,
		Enabled:             row.Enabled,
		State:               domain.State(row.State),
		ConsecutiveFailures: row.ConsecutiveFailures,
		LastCheckedAt:       row.LastCheckedAt,
		LastStatusCode:      row.LastStatusCode,
		LastLatencyMS:       row.LastLatencyMs,
		NextCheckAt:         row.NextCheckAt,
		LeaseOwner:          row.LeaseOwner,
		LeaseUntil:          row.LeaseUntil,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		DeletedAt:           row.DeletedAt,
	}
}

// constraintFields maps a database constraint name onto the request field that
// violated it. Application validation should catch all of these first; this is
// what turns the residue into a 422 instead of a 500 if it ever does not.
var constraintFields = map[string]string{
	"monitors_name_not_blank":            "name",
	"monitors_url_not_blank":             "url",
	"monitors_url_length":                "url",
	"monitors_interval_range":            "interval_seconds",
	"monitors_timeout_range":             "timeout_ms",
	"monitors_expected_status_min_range": "expected_status_min",
	"monitors_expected_status_max_range": "expected_status_max",
	"monitors_expected_status_order":     "expected_status_min",
	"monitors_failure_threshold_range":   "failure_threshold",
	"monitors_state_valid":               "state",
}

// pgCheckViolation is the SQLSTATE for a failed CHECK constraint.
const pgCheckViolation = "23514"

// translateError converts storage errors into the vocabulary the service layer
// understands, so that nothing above this package has to know about pgx.
func translateError(err error, op string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation {
		if field, ok := constraintFields[pgErr.ConstraintName]; ok {
			return &monitor.ValidationError{
				Fields: []monitor.FieldError{{
					Field:   field,
					Message: "violates database constraint " + pgErr.ConstraintName,
				}},
			}
		}
	}

	return fmt.Errorf("%s: %w", op, err)
}
