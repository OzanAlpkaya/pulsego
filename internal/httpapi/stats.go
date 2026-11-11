package httpapi

import (
	"net/http"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// statsResponse is the wire representation of a monitor's stats over one
// window. The nullable figures are pointers so that "no data" serialises as
// JSON null rather than as a misleading 0.
type statsResponse struct {
	Window           string    `json:"window"`
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	TotalChecks      int64     `json:"total_checks"`
	SuccessfulChecks int64     `json:"successful_checks"`
	UptimePercent    *float64  `json:"uptime_percent"`
	AvgLatencyMs     *float64  `json:"avg_latency_ms"`
	P95LatencyMs     *int32    `json:"p95_latency_ms"`
}

func newStatsResponse(s domain.Stats) statsResponse {
	return statsResponse{
		Window:           s.Window.String(),
		From:             s.From,
		To:               s.To,
		TotalChecks:      s.TotalChecks,
		SuccessfulChecks: s.SuccessfulChecks,
		UptimePercent:    s.UptimePercent,
		AvgLatencyMs:     s.AvgLatencyMs,
		P95LatencyMs:     s.P95LatencyMs,
	}
}

// stats returns a monitor's uptime and latency over one window.
func (h *MonitorHandler) stats(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	window := domain.DefaultStatsWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		parsed, err := domain.ParseStatsWindow(raw)
		if err != nil {
			problemInvalidQuery(w, r, h.log, &monitor.ValidationError{Fields: []monitor.FieldError{
				{Field: "window", Message: "must be one of 24h, 7d, 30d"},
			}})
			return
		}
		window = parsed
	}

	stats, err := h.service.Stats(r.Context(), id, window)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, r, h.log, http.StatusOK, newStatsResponse(stats))
}
