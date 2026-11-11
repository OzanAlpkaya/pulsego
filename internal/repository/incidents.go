package repository

import (
	"context"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
)

// ListIncidents returns one page of a monitor's incident history, newest
// first. Like ListChecks, it does not check that the monitor exists; the
// service does that first, so that a missing monitor is a 404 rather than an
// empty page.
func (s *MonitorStore) ListIncidents(ctx context.Context, p monitor.ListIncidentsParams) ([]domain.Incident, error) {
	params := sqlcgen.ListIncidentsParams{MonitorID: p.MonitorID, PageLimit: p.Limit}
	if p.Cursor != nil {
		params.CursorStartedAt = &p.Cursor.StartedAt
		params.CursorID = p.Cursor.ID
	}

	rows, err := s.q.ListIncidents(ctx, params)
	if err != nil {
		return nil, translateError(err, "list incidents")
	}

	incidents := make([]domain.Incident, 0, len(rows))
	for _, row := range rows {
		incidents = append(incidents, incidentToDomain(row))
	}
	return incidents, nil
}

// incidentToDomain maps a generated incidents row onto the domain type.
func incidentToDomain(row sqlcgen.Incident) domain.Incident {
	return domain.Incident{
		ID:             row.ID,
		MonitorID:      row.MonitorID,
		StartedAt:      row.StartedAt,
		ResolvedAt:     row.ResolvedAt,
		OpeningCheckID: row.OpeningCheckID,
		ClosingCheckID: row.ClosingCheckID,
		Cause:          row.Cause,
		CreatedAt:      row.CreatedAt,
	}
}
