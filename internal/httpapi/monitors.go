package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// maxRequestBody caps how much a client can send. Monitor definitions are a few
// hundred bytes; anything larger is a mistake or an attack.
const maxRequestBody = 64 << 10

// MonitorService is the behaviour the handlers need. Declared here so the
// transport layer depends on an interface it owns rather than on *monitor.Service.
type MonitorService interface {
	Create(ctx context.Context, in monitor.CreateInput) (domain.Monitor, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Monitor, error)
	List(ctx context.Context, limit int32, cursor *monitor.Cursor) ([]domain.Monitor, *monitor.Cursor, error)
	Update(ctx context.Context, id uuid.UUID, in monitor.UpdateInput) (domain.Monitor, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// MonitorHandler serves the /v1/monitors resource.
type MonitorHandler struct {
	service MonitorService
	log     *slog.Logger
}

// NewMonitorHandler wires a handler to its service.
func NewMonitorHandler(service MonitorService, log *slog.Logger) *MonitorHandler {
	return &MonitorHandler{service: service, log: log}
}

// Routes returns the monitor sub-router.
func (h *MonitorHandler) Routes() chi.Router {
	r := chi.NewRouter()

	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{monitorID}", h.get)
	r.Patch("/{monitorID}", h.update)
	r.Delete("/{monitorID}", h.delete)

	return r
}

// monitorResponse is the wire representation of a monitor. It is a separate
// type from domain.Monitor on purpose: the lease columns are internal
// scheduling state and are never exposed.
type monitorResponse struct {
	ID                  uuid.UUID  `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	IntervalSeconds     int32      `json:"interval_seconds"`
	TimeoutMS           int32      `json:"timeout_ms"`
	ExpectedStatusMin   int16      `json:"expected_status_min"`
	ExpectedStatusMax   int16      `json:"expected_status_max"`
	FailureThreshold    int16      `json:"failure_threshold"`
	Enabled             bool       `json:"enabled"`
	State               string     `json:"state"`
	ConsecutiveFailures int32      `json:"consecutive_failures"`
	LastCheckedAt       *time.Time `json:"last_checked_at"`
	LastStatusCode      *int32     `json:"last_status_code"`
	LastLatencyMS       *int32     `json:"last_latency_ms"`
	NextCheckAt         time.Time  `json:"next_check_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

func newMonitorResponse(m domain.Monitor) monitorResponse {
	return monitorResponse{
		ID:                  m.ID,
		Name:                m.Name,
		URL:                 m.URL,
		IntervalSeconds:     m.IntervalSeconds,
		TimeoutMS:           m.TimeoutMS,
		ExpectedStatusMin:   m.ExpectedStatusMin,
		ExpectedStatusMax:   m.ExpectedStatusMax,
		FailureThreshold:    m.FailureThreshold,
		Enabled:             m.Enabled,
		State:               m.State.String(),
		ConsecutiveFailures: m.ConsecutiveFailures,
		LastCheckedAt:       m.LastCheckedAt,
		LastStatusCode:      m.LastStatusCode,
		LastLatencyMS:       m.LastLatencyMS,
		NextCheckAt:         m.NextCheckAt,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
	}
}

// listResponse wraps a page. The envelope exists so that next_cursor has
// somewhere to live without a custom header.
type listResponse struct {
	Data       []monitorResponse `json:"data"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type createMonitorRequest struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	IntervalSeconds   *int32 `json:"interval_seconds"`
	TimeoutMS         *int32 `json:"timeout_ms"`
	ExpectedStatusMin *int16 `json:"expected_status_min"`
	ExpectedStatusMax *int16 `json:"expected_status_max"`
	FailureThreshold  *int16 `json:"failure_threshold"`
	Enabled           *bool  `json:"enabled"`
}

type updateMonitorRequest struct {
	Name              *string `json:"name"`
	URL               *string `json:"url"`
	IntervalSeconds   *int32  `json:"interval_seconds"`
	TimeoutMS         *int32  `json:"timeout_ms"`
	ExpectedStatusMin *int16  `json:"expected_status_min"`
	ExpectedStatusMax *int16  `json:"expected_status_max"`
	FailureThreshold  *int16  `json:"failure_threshold"`
	Enabled           *bool   `json:"enabled"`
}

func (h *MonitorHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createMonitorRequest
	if err := decodeJSON(w, r, &req); err != nil {
		problemMalformed(w, r, h.log, err.Error())
		return
	}

	created, err := h.service.Create(r.Context(), monitor.CreateInput{
		Name:              req.Name,
		URL:               req.URL,
		IntervalSeconds:   req.IntervalSeconds,
		TimeoutMS:         req.TimeoutMS,
		ExpectedStatusMin: req.ExpectedStatusMin,
		ExpectedStatusMax: req.ExpectedStatusMax,
		FailureThreshold:  req.FailureThreshold,
		Enabled:           req.Enabled,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	w.Header().Set("Location", "/v1/monitors/"+created.ID.String())
	writeJSON(w, r, h.log, http.StatusCreated, newMonitorResponse(created))
}

func (h *MonitorHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	found, err := h.service.Get(r.Context(), id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, r, h.log, http.StatusOK, newMonitorResponse(found))
}

func (h *MonitorHandler) list(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		problemMalformed(w, r, h.log, err.Error())
		return
	}

	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		problemMalformed(w, r, h.log, "the cursor parameter is not a cursor issued by this API")
		return
	}

	monitors, next, err := h.service.List(r.Context(), limit, cursor)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	data := make([]monitorResponse, 0, len(monitors))
	for _, m := range monitors {
		data = append(data, newMonitorResponse(m))
	}

	writeJSON(w, r, h.log, http.StatusOK, listResponse{Data: data, NextCursor: encodeCursor(next)})
}

func (h *MonitorHandler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	var req updateMonitorRequest
	if err := decodeJSON(w, r, &req); err != nil {
		problemMalformed(w, r, h.log, err.Error())
		return
	}

	in := monitor.UpdateInput(req)
	if in.IsEmpty() {
		problemMalformed(w, r, h.log, "the request body must change at least one field")
		return
	}

	updated, err := h.service.Update(r.Context(), id, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, r, h.log, http.StatusOK, newMonitorResponse(updated))
}

func (h *MonitorHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.monitorID(w, r)
	if !ok {
		return
	}

	if err := h.service.Delete(r.Context(), id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// monitorID parses the path parameter, answering the request itself when the
// value is not a UUID.
func (h *MonitorHandler) monitorID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := chi.URLParam(r, "monitorID")

	id, err := uuid.Parse(raw)
	if err != nil {
		problemMalformed(w, r, h.log, "the monitor id in the path is not a valid UUID")
		return uuid.Nil, false
	}
	return id, true
}

// writeServiceError maps a service error onto the right problem document.
func (h *MonitorHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *monitor.ValidationError

	switch {
	case errors.Is(err, domain.ErrNotFound):
		problemNotFound(w, r, h.log, "no monitor exists with that id")
	case errors.As(err, &verr):
		problemValidation(w, r, h.log, verr)
	default:
		problemInternal(w, r, h.log, err)
	}
}

// decodeJSON reads a request body strictly: unknown fields are rejected so a
// misspelled key fails loudly instead of being silently ignored, and the body
// is capped.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return describeDecodeError(err)
	}

	// A second value in the body means the client sent something other than a
	// single JSON object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain a single JSON object")
	}
	return nil
}

// describeDecodeError turns encoding/json's internal errors into messages a
// caller can act on.
func describeDecodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)

	switch {
	case errors.Is(err, io.EOF):
		return errors.New("the request body is empty")
	case errors.As(err, &syntaxErr):
		return fmt.Errorf("the request body is not valid JSON (at byte %d)", syntaxErr.Offset)
	case errors.As(err, &typeErr):
		return fmt.Errorf("the field %q has the wrong type, expected %s", typeErr.Field, typeErr.Type)
	case errors.As(err, &maxErr):
		return fmt.Errorf("the request body must not exceed %d bytes", maxErr.Limit)
	default:
		// DisallowUnknownFields reports through a plain error whose text names
		// the offending key, which is exactly what the caller needs.
		return err
	}
}

// parseLimit reads the page size, rejecting values that are not usable rather
// than silently clamping a caller's typo.
func parseLimit(raw string) (int32, error) {
	if raw == "" {
		return monitor.DefaultPageSize, nil
	}

	// ParseInt with a bit size of 32 is what makes the result provably fit the
	// int32 the service takes.
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, errors.New("the limit parameter must be an integer")
	}
	if n < 1 || n > monitor.MaxPageSize {
		return 0, fmt.Errorf("the limit parameter must be between 1 and %d", monitor.MaxPageSize)
	}
	return int32(n), nil
}

func writeJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.ErrorContext(r.Context(), "encoding response failed", slog.Any("error", err))
	}
}
