package domain_test

import (
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func TestParseStatsWindow(t *testing.T) {
	t.Parallel()

	valid := map[string]time.Duration{
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	for raw, want := range valid {
		w, err := domain.ParseStatsWindow(raw)
		if err != nil {
			t.Errorf("ParseStatsWindow(%q) returned an unexpected error: %v", raw, err)
			continue
		}
		if w.String() != raw {
			t.Errorf("ParseStatsWindow(%q) = %q", raw, w)
		}
		if got := w.Duration(); got != want {
			t.Errorf("%q.Duration() = %v, want %v", raw, got, want)
		}
	}

	for _, raw := range []string{"", "24H", " 24h", "1d", "1h", "30", "7days", "-7d", "90d"} {
		if w, err := domain.ParseStatsWindow(raw); err == nil {
			t.Errorf("ParseStatsWindow(%q) = %q, want an error", raw, w)
		}
	}

	if got := domain.StatsWindow("1d").Duration(); got != 0 {
		t.Errorf("unknown window Duration() = %v, want 0", got)
	}
	if domain.DefaultStatsWindow != domain.StatsWindow24h {
		t.Errorf("DefaultStatsWindow = %q, want 24h", domain.DefaultStatsWindow)
	}
	if len(domain.StatsWindows) != len(valid) {
		t.Errorf("StatsWindows has %d entries, want %d", len(domain.StatsWindows), len(valid))
	}
}

func TestNewStats(t *testing.T) {
	t.Parallel()

	f64 := func(v float64) *float64 { return &v }
	i32 := func(v int32) *int32 { return &v }
	end := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		raw        domain.RawStats
		wantUptime *float64
		wantAvg    *float64
		wantP95    *int32
	}{
		{
			name:       "all successful",
			raw:        domain.RawStats{TotalChecks: 4, SuccessfulChecks: 4, AvgLatencyMs: f64(120), P95LatencyMs: i32(180)},
			wantUptime: f64(100), wantAvg: f64(120), wantP95: i32(180),
		},
		{
			name:       "exact fraction is kept",
			raw:        domain.RawStats{TotalChecks: 8, SuccessfulChecks: 7, AvgLatencyMs: f64(10), P95LatencyMs: i32(10)},
			wantUptime: f64(87.5), wantAvg: f64(10), wantP95: i32(10),
		},
		{
			name:       "uptime and avg round to two decimals",
			raw:        domain.RawStats{TotalChecks: 3, SuccessfulChecks: 2, AvgLatencyMs: f64(200.3333333), P95LatencyMs: i32(301)},
			wantUptime: f64(66.67), wantAvg: f64(200.33), wantP95: i32(301),
		},
		{
			name:       "avg rounds up at the third decimal",
			raw:        domain.RawStats{TotalChecks: 7, SuccessfulChecks: 1, AvgLatencyMs: f64(341.8366), P95LatencyMs: i32(400)},
			wantUptime: f64(14.29), wantAvg: f64(341.84), wantP95: i32(400),
		},
		{
			name:       "near-total uptime may round to 100",
			raw:        domain.RawStats{TotalChecks: 100000, SuccessfulChecks: 99999, AvgLatencyMs: f64(1), P95LatencyMs: i32(1)},
			wantUptime: f64(100), wantAvg: f64(1), wantP95: i32(1),
		},
		{
			name: "zero checks: everything null",
			raw:  domain.RawStats{},
		},
		{
			name:       "all failed: uptime 0, latency null",
			raw:        domain.RawStats{TotalChecks: 5, SuccessfulChecks: 0},
			wantUptime: f64(0),
		},
		{
			name:       "all failed ignores latency the query should not have returned",
			raw:        domain.RawStats{TotalChecks: 5, SuccessfulChecks: 0, AvgLatencyMs: f64(5000), P95LatencyMs: i32(5000)},
			wantUptime: f64(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.raw.WindowEnd = end
			got := domain.NewStats(domain.StatsWindow7d, tt.raw)

			if got.Window != domain.StatsWindow7d {
				t.Errorf("Window = %q, want 7d", got.Window)
			}
			if !got.To.Equal(end) {
				t.Errorf("To = %v, want %v", got.To, end)
			}
			if want := end.Add(-7 * 24 * time.Hour); !got.From.Equal(want) {
				t.Errorf("From = %v, want %v", got.From, want)
			}
			if got.TotalChecks != tt.raw.TotalChecks || got.SuccessfulChecks != tt.raw.SuccessfulChecks {
				t.Errorf("counts = %d/%d, want %d/%d",
					got.SuccessfulChecks, got.TotalChecks, tt.raw.SuccessfulChecks, tt.raw.TotalChecks)
			}
			assertPtr(t, "UptimePercent", got.UptimePercent, tt.wantUptime)
			assertPtr(t, "AvgLatencyMs", got.AvgLatencyMs, tt.wantAvg)
			assertPtr(t, "P95LatencyMs", got.P95LatencyMs, tt.wantP95)
		})
	}
}

func assertPtr[T comparable](t *testing.T, name string, got, want *T) {
	t.Helper()

	switch {
	case got == nil && want == nil:
	case got == nil:
		t.Errorf("%s = nil, want %v", name, *want)
	case want == nil:
		t.Errorf("%s = %v, want nil", name, *got)
	case *got != *want:
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}
