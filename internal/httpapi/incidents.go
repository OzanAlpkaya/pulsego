package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

// incidentResponse is the wire representation of one incident. Nullable
// columns are pointers so that they serialise as JSON null: resolved_at and
// closing_check_id while the incident is open, and the check references once
// retention has deleted those checks.
type incidentResponse struct {
	ID             uuid.UUID  `json:"id"`
	MonitorID      uuid.UUID  `json:"monitor_id"`
	StartedAt      time.Time  `json:"started_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	OpeningCheckID *int64     `json:"opening_check_id"`
	ClosingCheckID *int64     `json:"closing_check_id"`
	Cause          *string    `json:"cause"`
	CreatedAt      time.Time  `json:"created_at"`
}

func newIncidentResponse(i domain.Incident) incidentResponse {
	return incidentResponse{
		ID:             i.ID,
		MonitorID:      i.MonitorID,
		StartedAt:      i.StartedAt,
		ResolvedAt:     i.ResolvedAt,
		OpeningCheckID: i.OpeningCheckID,
		ClosingCheckID: i.ClosingCheckID,
		Cause:          i.Cause,
		CreatedAt:      i.CreatedAt,
	}
}

// incidentListResponse wraps a page of incident history in the same envelope
// as the monitor and check lists.
type incidentListResponse struct {
	Data       []incidentResponse `json:"data"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// listIncidents returns one page of a monitor's incident history, newest
// first.
func (h *MonitorHandler) listIncidents(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		problemMalformed(w, r, h.log, err.Error())
		return
	}

	cursor, err := decodeIncidentCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		problemMalformed(w, r, h.log, "the cursor parameter is not an incident history cursor issued by this API")
		return
	}

	incidents, next, err := h.service.ListIncidents(r.Context(), id, limit, cursor)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	data := make([]incidentResponse, 0, len(incidents))
	for _, i := range incidents {
		data = append(data, newIncidentResponse(i))
	}

	writeJSON(w, r, h.log, http.StatusOK, incidentListResponse{Data: data, NextCursor: encodeIncidentCursor(next)})
}
