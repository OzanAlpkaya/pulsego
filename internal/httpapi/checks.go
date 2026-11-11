package httpapi

import (
	"net/http"
	"time"
)

// checkResponse is the wire representation of one stored check. The monitor id
// is omitted: the history endpoint is already scoped to one monitor.
type checkResponse struct {
	ID           int64     `json:"id"`
	Success      bool      `json:"success"`
	StatusCode   *int32    `json:"status_code"`
	LatencyMS    int32     `json:"latency_ms"`
	ErrorKind    *string   `json:"error_kind"`
	ErrorMessage *string   `json:"error_message"`
	CheckedAt    time.Time `json:"checked_at"`
}

// checkListResponse wraps a page of check history in the same envelope as the
// monitor list.
type checkListResponse struct {
	Data       []checkResponse `json:"data"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// requestCheck makes a monitor due now. The check itself runs asynchronously on
// a worker, hence 202 rather than 200: the response says the request was
// accepted, not that a result exists.
func (h *MonitorHandler) requestCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	requested, err := h.service.RequestCheck(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, r, h.log, http.StatusAccepted, newMonitorResponse(requested))
}

// listChecks returns one page of a monitor's check history, newest first.
func (h *MonitorHandler) listChecks(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		problemMalformed(w, r, h.log, err.Error())
		return
	}

	cursor, err := decodeCheckCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		problemMalformed(w, r, h.log, "the cursor parameter is not a check history cursor issued by this API")
		return
	}

	checks, next, err := h.service.ListChecks(r.Context(), id, limit, cursor)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	data := make([]checkResponse, 0, len(checks))
	for _, c := range checks {
		resp := checkResponse{
			ID:           c.ID,
			Success:      c.Success,
			StatusCode:   c.StatusCode,
			LatencyMS:    c.LatencyMS,
			ErrorMessage: c.ErrorMessage,
			CheckedAt:    c.CheckedAt,
		}
		if c.ErrorKind != nil {
			kind := c.ErrorKind.String()
			resp.ErrorKind = &kind
		}
		data = append(data, resp)
	}

	writeJSON(w, r, h.log, http.StatusOK, checkListResponse{Data: data, NextCursor: encodeCheckCursor(next)})
}
