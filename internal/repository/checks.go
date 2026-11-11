package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
)

// RequestCheck makes a monitor due now so that the next poll claims it. It
// returns domain.ErrNotFound if the monitor is absent or soft-deleted.
func (s *MonitorStore) RequestCheck(ctx context.Context, id uuid.UUID) (domain.Monitor, error) {
	row, err := s.q.RequestCheck(ctx, id)
	if err != nil {
		return domain.Monitor{}, translateError(err, "request check")
	}
	return toDomain(row), nil
}

// ListChecks returns one page of a monitor's check history, newest first. It
// does not check that the monitor exists; the service does that first, so
// that a missing monitor is a 404 rather than an empty page.
func (s *MonitorStore) ListChecks(ctx context.Context, p monitor.ListChecksParams) ([]domain.Check, error) {
	params := sqlcgen.ListChecksParams{MonitorID: p.MonitorID, PageLimit: p.Limit}
	if p.Cursor != nil {
		params.CursorCheckedAt = &p.Cursor.CheckedAt
		params.CursorID = p.Cursor.ID
	}

	rows, err := s.q.ListChecks(ctx, params)
	if err != nil {
		return nil, translateError(err, "list checks")
	}

	checks := make([]domain.Check, 0, len(rows))
	for _, row := range rows {
		checks = append(checks, checkToDomain(row))
	}
	return checks, nil
}

// checkToDomain maps a generated monitor_checks row onto the domain type.
func checkToDomain(row sqlcgen.MonitorCheck) domain.Check {
	var kind *domain.ErrorKind
	if row.ErrorKind != nil {
		k := domain.ErrorKind(*row.ErrorKind)
		kind = &k
	}

	return domain.Check{
		CheckOutcome: domain.CheckOutcome{
			Success:      row.Success,
			StatusCode:   row.StatusCode,
			LatencyMS:    row.LatencyMs,
			ErrorKind:    kind,
			ErrorMessage: row.ErrorMessage,
		},
		ID:        row.ID,
		MonitorID: row.MonitorID,
		CheckedAt: row.CheckedAt,
	}
}
