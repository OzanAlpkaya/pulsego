package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

func TestStats(t *testing.T) {
	t.Parallel()

	t.Run("derives the summary from the raw aggregate", func(t *testing.T) {
		t.Parallel()

		id := uuid.New()
		store := newFakeStore()
		store.monitors[id] = domain.Monitor{ID: id, Enabled: true}
		avg, p95 := 200.3333, int32(301)
		end := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
		store.rawStats = domain.RawStats{
			TotalChecks: 4, SuccessfulChecks: 3, AvgLatencyMs: &avg, P95LatencyMs: &p95, WindowEnd: end,
		}
		svc := monitor.NewService(store, testDefaults())

		got, err := svc.Stats(context.Background(), id, domain.StatsWindow30d)
		if err != nil {
			t.Fatalf("Stats() returned an unexpected error: %v", err)
		}
		if store.statsMonitorID != id || store.statsWindow != domain.StatsWindow30d {
			t.Errorf("store queried for %v/%q, want %v/30d", store.statsMonitorID, store.statsWindow, id)
		}
		if got.UptimePercent == nil || *got.UptimePercent != 75 {
			t.Errorf("UptimePercent = %v, want 75", got.UptimePercent)
		}
		if got.AvgLatencyMs == nil || *got.AvgLatencyMs != 200.33 {
			t.Errorf("AvgLatencyMs = %v, want 200.33", got.AvgLatencyMs)
		}
		if !got.From.Equal(end.Add(-30 * 24 * time.Hour)) {
			t.Errorf("From = %v, want 30 days before %v", got.From, end)
		}
	})

	t.Run("missing monitor is not found", func(t *testing.T) {
		t.Parallel()

		store := newFakeStore()
		svc := monitor.NewService(store, testDefaults())

		_, err := svc.Stats(context.Background(), uuid.New(), domain.StatsWindow24h)
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("error = %v, want domain.ErrNotFound", err)
		}
		if store.statsWindow != "" {
			t.Error("the store was queried for the stats of a missing monitor")
		}
	})
}
