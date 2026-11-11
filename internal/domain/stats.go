package domain

import (
	"fmt"
	"math"
	"time"
)

// StatsWindow is one of the fixed periods the stats endpoint aggregates over.
// Windows end at the database's now() and reach back by their Duration; the
// start is exclusive and the end inclusive.
type StatsWindow string

// The supported windows. Nothing else is accepted: an arbitrary duration would
// let a caller ask for a range that retention has already deleted, or scan far
// more rows than the three dashboards need.
const (
	StatsWindow24h StatsWindow = "24h"
	StatsWindow7d  StatsWindow = "7d"
	StatsWindow30d StatsWindow = "30d"
)

// DefaultStatsWindow is used when a caller does not name a window.
const DefaultStatsWindow = StatsWindow24h

// StatsWindows lists every supported window, shortest first.
var StatsWindows = []StatsWindow{StatsWindow24h, StatsWindow7d, StatsWindow30d}

// statsDecimals is how many decimal places uptime_percent and avg_latency_ms
// are rounded to.
const statsDecimals = 2

// ParseStatsWindow returns the window named by s. Matching is exact: "24H",
// " 24h" and "1d" are all errors.
func ParseStatsWindow(s string) (StatsWindow, error) {
	switch w := StatsWindow(s); w {
	case StatsWindow24h, StatsWindow7d, StatsWindow30d:
		return w, nil
	default:
		return "", fmt.Errorf("unknown stats window %q: must be one of 24h, 7d, 30d", s)
	}
}

// Duration is the length of the window. It is zero for a value that did not
// come from ParseStatsWindow or one of the constants.
func (w StatsWindow) Duration() time.Duration {
	const day = 24 * time.Hour
	switch w {
	case StatsWindow24h:
		return day
	case StatsWindow7d:
		return 7 * day
	case StatsWindow30d:
		return 30 * day
	default:
		return 0
	}
}

func (w StatsWindow) String() string { return string(w) }

// RawStats is what the database aggregates over one window of a monitor's
// checks. The latency figures cover successful checks only, and are nil when
// none succeeded. P95LatencyMs comes from percentile_disc, so it is a latency
// that was actually observed, in whole milliseconds.
type RawStats struct {
	TotalChecks      int64
	SuccessfulChecks int64
	AvgLatencyMs     *float64
	P95LatencyMs     *int32
	// WindowEnd is the database's now() when the aggregate was taken.
	WindowEnd time.Time
}

// Stats is the availability and latency summary of one monitor over one
// window, as the API returns it.
type Stats struct {
	Window           StatsWindow
	From             time.Time
	To               time.Time
	TotalChecks      int64
	SuccessfulChecks int64
	// UptimePercent is nil when the window holds no checks: no data is not
	// the same as 0% or 100% uptime.
	UptimePercent *float64
	// AvgLatencyMs and P95LatencyMs are nil when no check in the window
	// succeeded.
	AvgLatencyMs *float64
	P95LatencyMs *int32
}

// NewStats derives the API summary from the raw aggregate: From is WindowEnd
// minus the window, uptime is successful / total * 100, and uptime and the
// average latency are rounded to two decimal places. It is pure, so the null
// rules are enforced here rather than trusted to the query.
func NewStats(window StatsWindow, raw RawStats) Stats {
	s := Stats{
		Window:           window,
		From:             raw.WindowEnd.Add(-window.Duration()),
		To:               raw.WindowEnd,
		TotalChecks:      raw.TotalChecks,
		SuccessfulChecks: raw.SuccessfulChecks,
	}

	if raw.TotalChecks > 0 {
		uptime := roundStat(float64(raw.SuccessfulChecks) / float64(raw.TotalChecks) * 100)
		s.UptimePercent = &uptime
	}

	if raw.SuccessfulChecks > 0 {
		if raw.AvgLatencyMs != nil {
			avg := roundStat(*raw.AvgLatencyMs)
			s.AvgLatencyMs = &avg
		}
		if raw.P95LatencyMs != nil {
			p95 := *raw.P95LatencyMs
			s.P95LatencyMs = &p95
		}
	}

	return s
}

// roundStat rounds half away from zero to statsDecimals places.
func roundStat(v float64) float64 {
	scale := math.Pow10(statsDecimals)
	return math.Round(v*scale) / scale
}
