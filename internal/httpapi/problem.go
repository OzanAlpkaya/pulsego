package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// ContentTypeProblem is the media type mandated by RFC 7807.
const ContentTypeProblem = "application/problem+json"

// Problem types. These are stable identifiers, not links: URNs are used rather
// than URLs so that nothing implies a documentation page that does not exist.
const (
	TypeValidation   = "urn:pulsego:problem:validation-error"
	TypeMalformed    = "urn:pulsego:problem:malformed-request"
	TypeUnauthorized = "urn:pulsego:problem:unauthorized"
	TypeNotFound     = "urn:pulsego:problem:not-found"
	TypeInternal     = "urn:pulsego:problem:internal-error"
)

// Problem is an RFC 7807 error document. Errors is an extension member listing
// the individual field failures behind a validation error.
type Problem struct {
	Type      string               `json:"type"`
	Title     string               `json:"title"`
	Status    int                  `json:"status"`
	Detail    string               `json:"detail,omitempty"`
	RequestID string               `json:"request_id,omitempty"`
	Errors    []monitor.FieldError `json:"errors,omitempty"`
}

// writeProblem renders an error document. The request ID is pulled from the
// context so that a caller reporting a failure can be matched to its log line.
func writeProblem(w http.ResponseWriter, r *http.Request, log *slog.Logger, p Problem) {
	p.RequestID = middleware.GetReqID(r.Context())

	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(p.Status)

	if err := json.NewEncoder(w).Encode(p); err != nil {
		// The status line is already sent, so this can only be logged.
		log.ErrorContext(r.Context(), "encoding problem response failed", slog.Any("error", err))
	}
}

func problemNotFound(w http.ResponseWriter, r *http.Request, log *slog.Logger, detail string) {
	writeProblem(w, r, log, Problem{
		Type:   TypeNotFound,
		Title:  "Not Found",
		Status: http.StatusNotFound,
		Detail: detail,
	})
}

func problemMalformed(w http.ResponseWriter, r *http.Request, log *slog.Logger, detail string) {
	writeProblem(w, r, log, Problem{
		Type:   TypeMalformed,
		Title:  "Malformed Request",
		Status: http.StatusBadRequest,
		Detail: detail,
	})
}

func problemValidation(w http.ResponseWriter, r *http.Request, log *slog.Logger, verr *monitor.ValidationError) {
	writeProblem(w, r, log, Problem{
		Type:   TypeValidation,
		Title:  "Validation Failed",
		Status: http.StatusUnprocessableEntity,
		Detail: "The request body was well-formed but one or more fields are invalid.",
		Errors: verr.Fields,
	})
}

// problemInternal deliberately reveals nothing about the cause. The detail goes
// to the log, keyed by the same request ID the caller receives.
func problemInternal(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	log.ErrorContext(r.Context(), "request failed",
		slog.Any("error", err),
		slog.String("request_id", middleware.GetReqID(r.Context())),
	)

	writeProblem(w, r, log, Problem{
		Type:   TypeInternal,
		Title:  "Internal Server Error",
		Status: http.StatusInternalServerError,
		Detail: "The request could not be completed. The request_id identifies this failure in the server logs.",
	})
}
