package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/repository/sqlcgen"
)

// GetStats aggregates a monitor's checks over the window ending at the
// database's now(). Like ListChecks, it does not check that the monitor
// exists; the service does that first, so that a missing monitor is a 404
// rather than a window with no checks.
func (s *MonitorStore) GetStats(ctx context.Context, monitorID uuid.UUID, window domain.StatsWindow) (domain.RawStats, error) {
	row, err := s.q.GetMonitorStats(ctx, sqlcgen.GetMonitorStatsParams{
		MonitorID:     monitorID,
		WindowSeconds: window.Duration().Seconds(),
	})
	if err != nil {
		return domain.RawStats{}, translateError(err, "get monitor stats")
	}

	return domain.RawStats{
		TotalChecks:      row.TotalChecks,
		SuccessfulChecks: row.SuccessfulChecks,
		AvgLatencyMs:     row.AvgLatencyMs,
		P95LatencyMs:     row.P95LatencyMs,
		WindowEnd:        row.WindowEnd,
	}, nil
}
