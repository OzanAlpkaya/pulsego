package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// stubService stands in for the monitor service so handler behaviour can be
// tested independently of validation and storage.
type stubService struct {
	monitor domain.Monitor
	page    []domain.Monitor
	next    *monitor.Cursor
	err     error

	createInput *monitor.CreateInput
	updateInput *monitor.UpdateInput
	gotLimit    int32
	gotCursor   *monitor.Cursor
	deletedID   uuid.UUID

	checks         []domain.Check
	checksNext     *monitor.CheckCursor
	requestedID    uuid.UUID
	gotCheckLimit  int32
	gotCheckCursor *monitor.CheckCursor
}

func (s *stubService) Create(_ context.Context, in monitor.CreateInput) (domain.Monitor, error) {
	s.createInput = &in
	return s.monitor, s.err
}

func (s *stubService) Get(_ context.Context, _ uuid.UUID) (domain.Monitor, error) {
	return s.monitor, s.err
}

func (s *stubService) List(_ context.Context, limit int32, cursor *monitor.Cursor) ([]domain.Monitor, *monitor.Cursor, error) {
	s.gotLimit, s.gotCursor = limit, cursor
	return s.page, s.next, s.err
}

func (s *stubService) Update(_ context.Context, _ uuid.UUID, in monitor.UpdateInput) (domain.Monitor, error) {
	s.updateInput = &in
	return s.monitor, s.err
}

func (s *stubService) Delete(_ context.Context, id uuid.UUID) error {
	s.deletedID = id
	return s.err
}

func (s *stubService) RequestCheck(_ context.Context, id uuid.UUID) (domain.Monitor, error) {
	s.requestedID = id
	return s.monitor, s.err
}

func (s *stubService) ListChecks(_ context.Context, _ uuid.UUID, limit int32, cursor *monitor.CheckCursor) ([]domain.Check, *monitor.CheckCursor, error) {
	s.gotCheckLimit, s.gotCheckCursor = limit, cursor
	return s.checks, s.checksNext, s.err
}

func newTestRouter(t *testing.T, svc httpapi.MonitorService) http.Handler {
	t.Helper()

	return httpapi.NewRouter(
		httpapi.RouterConfig{APIKey: testAPIKey, Monitors: svc},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
}

// authedRequest builds a request that will pass the API key middleware.
func authedRequest(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()

	var reader io.Reader
	switch b := body.(type) {
	case nil:
		reader = nil
	case string:
		reader = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshalling the request body failed: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func sampleMonitor() domain.Monitor {
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	return domain.Monitor{
		ID:   uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		Name: "Example", URL: "https://example.com",
		IntervalSeconds: 60, TimeoutMS: 5000,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 3,
		Enabled: true, State: domain.StateUnknown,
		NextCheckAt: now, CreatedAt: now, UpdatedAt: now,
		LeaseOwner: strPtr("worker-1"), LeaseUntil: &now,
	}
}

func strPtr(s string) *string { return &s }

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) httpapi.Problem {
	t.Helper()

	if got, want := rec.Header().Get("Content-Type"), httpapi.ContentTypeProblem; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var p httpapi.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %q)", err, rec.Body.String())
	}
	if p.Status != rec.Code {
		t.Errorf("problem status field = %d, want it to match the HTTP status %d", p.Status, rec.Code)
	}
	if p.Title == "" {
		t.Error("problem title is empty")
	}
	if p.RequestID == "" {
		t.Error("problem request_id is empty")
	}
	return p
}

func TestCreateMonitor(t *testing.T) {
	t.Parallel()

	svc := &stubService{monitor: sampleMonitor()}
	rec := httptest.NewRecorder()

	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodPost, "/v1/monitors", map[string]any{
		"name": "Example",
		"url":  "https://example.com",
	}))

	if got, want := rec.Code, http.StatusCreated; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/v1/monitors/"+sampleMonitor().ID.String(); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if svc.createInput == nil {
		t.Fatal("the handler did not call the service")
	}
	if svc.createInput.Name != "Example" {
		t.Errorf("service received name %q, want %q", svc.createInput.Name, "Example")
	}
	if svc.createInput.IntervalSeconds != nil {
		t.Error("an omitted interval reached the service as non-nil, which would defeat defaulting")
	}
}

// TestResponseOmitsLeaseFields keeps internal scheduling state out of the API.
func TestResponseOmitsLeaseFields(t *testing.T) {
	t.Parallel()

	svc := &stubService{monitor: sampleMonitor()}
	rec := httptest.NewRecorder()

	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet,
		"/v1/monitors/11111111-2222-3333-4444-555555555555", nil))

	body := rec.Body.String()
	for _, leaked := range []string{"lease_owner", "lease_until", "worker-1", "deleted_at"} {
		if strings.Contains(body, leaked) {
			t.Errorf("response exposes %q: %s", leaked, body)
		}
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	for _, want := range []string{"id", "name", "url", "state", "next_check_at", "created_at", "updated_at"} {
		if _, ok := got[want]; !ok {
			t.Errorf("response is missing the %q field", want)
		}
	}
}

func TestCreateMonitorRejectsBadBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     any
		wantText string
	}{
		{name: "empty body", body: "", wantText: "empty"},
		{name: "invalid json", body: "{not json", wantText: "not valid JSON"},
		{name: "wrong field type", body: `{"name": 42, "url": "https://example.com"}`, wantText: "wrong type"},
		{name: "unknown field", body: `{"name":"a","url":"https://e.com","colour":"red"}`, wantText: "colour"},
		{name: "two objects", body: `{"name":"a","url":"https://e.com"}{"name":"b"}`, wantText: "single JSON object"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			newTestRouter(t, &stubService{}).ServeHTTP(rec,
				authedRequest(t, http.MethodPost, "/v1/monitors", tt.body))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
			}
			p := decodeProblem(t, rec)
			if p.Type != httpapi.TypeMalformed {
				t.Errorf("type = %q, want %q", p.Type, httpapi.TypeMalformed)
			}
			if !strings.Contains(p.Detail, tt.wantText) {
				t.Errorf("detail = %q, want it to mention %q", p.Detail, tt.wantText)
			}
		})
	}
}

// TestValidationErrorBecomes422 checks the shape the DoD calls for: a
// problem+json document carrying every rejected field.
func TestValidationErrorBecomes422(t *testing.T) {
	t.Parallel()

	svc := &stubService{err: &monitor.ValidationError{Fields: []monitor.FieldError{
		{Field: "name", Message: "is required"},
		{Field: "url", Message: "must use the http or https scheme"},
	}}}

	rec := httptest.NewRecorder()
	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodPost, "/v1/monitors",
		map[string]any{"name": "", "url": "ftp://example.com"}))

	if got, want := rec.Code, http.StatusUnprocessableEntity; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	p := decodeProblem(t, rec)
	if p.Type != httpapi.TypeValidation {
		t.Errorf("type = %q, want %q", p.Type, httpapi.TypeValidation)
	}
	if len(p.Errors) != 2 {
		t.Fatalf("errors has %d entries, want 2: %+v", len(p.Errors), p.Errors)
	}
	if p.Errors[0].Field != "name" || p.Errors[1].Field != "url" {
		t.Errorf("errors = %+v, want entries for name and url", p.Errors)
	}
}

func TestNotFoundBecomes404(t *testing.T) {
	t.Parallel()

	for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			svc := &stubService{err: domain.ErrNotFound}
			rec := httptest.NewRecorder()

			var body any
			if method == http.MethodPatch {
				body = map[string]any{"name": "new"}
			}
			newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, method,
				"/v1/monitors/11111111-2222-3333-4444-555555555555", body))

			if got, want := rec.Code, http.StatusNotFound; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			if p := decodeProblem(t, rec); p.Type != httpapi.TypeNotFound {
				t.Errorf("type = %q, want %q", p.Type, httpapi.TypeNotFound)
			}
		})
	}
}

func TestMalformedUUIDBecomes400(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newTestRouter(t, &stubService{}).ServeHTTP(rec,
		authedRequest(t, http.MethodGet, "/v1/monitors/not-a-uuid", nil))

	if got, want := rec.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if p := decodeProblem(t, rec); !strings.Contains(p.Detail, "UUID") {
		t.Errorf("detail = %q, want it to mention the UUID", p.Detail)
	}
}

// TestInternalErrorLeaksNothing keeps storage details out of client responses.
func TestInternalErrorLeaksNothing(t *testing.T) {
	t.Parallel()

	svc := &stubService{err: errStorage}
	rec := httptest.NewRecorder()

	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet,
		"/v1/monitors/11111111-2222-3333-4444-555555555555", nil))

	if got, want := rec.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if body := rec.Body.String(); strings.Contains(body, errStorage.Error()) {
		t.Errorf("response leaked the underlying error: %s", body)
	}
	if p := decodeProblem(t, rec); p.RequestID == "" {
		t.Error("a 500 must still carry a request_id so the failure can be traced")
	}
}

func TestUpdateMonitor(t *testing.T) {
	t.Parallel()

	svc := &stubService{monitor: sampleMonitor()}
	rec := httptest.NewRecorder()

	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodPatch,
		"/v1/monitors/11111111-2222-3333-4444-555555555555",
		map[string]any{"enabled": false}))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
	}
	if svc.updateInput == nil {
		t.Fatal("the handler did not call the service")
	}
	if svc.updateInput.Enabled == nil || *svc.updateInput.Enabled {
		t.Error("an explicit false did not reach the service as a set pointer")
	}
	if svc.updateInput.Name != nil {
		t.Error("an omitted field reached the service as non-nil")
	}
}

func TestUpdateRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newTestRouter(t, &stubService{}).ServeHTTP(rec, authedRequest(t, http.MethodPatch,
		"/v1/monitors/11111111-2222-3333-4444-555555555555", map[string]any{}))

	if got, want := rec.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if p := decodeProblem(t, rec); !strings.Contains(p.Detail, "at least one field") {
		t.Errorf("detail = %q, want it to explain that a change is required", p.Detail)
	}
}

func TestDeleteMonitorReturns204(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	svc := &stubService{}
	rec := httptest.NewRecorder()

	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodDelete, "/v1/monitors/"+id.String(), nil))

	if got, want := rec.Code, http.StatusNoContent; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 response has a body: %q", rec.Body.String())
	}
	if svc.deletedID != id {
		t.Errorf("service received %v, want %v", svc.deletedID, id)
	}
}

func TestListMonitors(t *testing.T) {
	t.Parallel()

	t.Run("returns a page and a cursor", func(t *testing.T) {
		t.Parallel()

		m := sampleMonitor()
		svc := &stubService{
			page: []domain.Monitor{m},
			next: &monitor.Cursor{CreatedAt: m.CreatedAt, ID: m.ID},
		}

		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, "/v1/monitors", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}

		var body struct {
			Data       []map[string]any `json:"data"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if len(body.Data) != 1 {
			t.Errorf("data has %d entries, want 1", len(body.Data))
		}
		if body.NextCursor == "" {
			t.Error("next_cursor is empty despite the service returning one")
		}
	})

	t.Run("omits the cursor on the last page", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{page: []domain.Monitor{}}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, "/v1/monitors", nil))

		if strings.Contains(rec.Body.String(), "next_cursor") {
			t.Errorf("next_cursor is present on the last page: %s", rec.Body.String())
		}
		// An empty page must still serialise as [] rather than null.
		if !strings.Contains(rec.Body.String(), `"data":[]`) {
			t.Errorf("empty page did not serialise as []: %s", rec.Body.String())
		}
	})

	t.Run("round-trips a cursor", func(t *testing.T) {
		t.Parallel()

		m := sampleMonitor()
		issuing := &stubService{page: []domain.Monitor{m}, next: &monitor.Cursor{CreatedAt: m.CreatedAt, ID: m.ID}}

		rec := httptest.NewRecorder()
		newTestRouter(t, issuing).ServeHTTP(rec, authedRequest(t, http.MethodGet, "/v1/monitors", nil))

		var body struct {
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}

		receiving := &stubService{}
		rec2 := httptest.NewRecorder()
		newTestRouter(t, receiving).ServeHTTP(rec2,
			authedRequest(t, http.MethodGet, "/v1/monitors?cursor="+body.NextCursor, nil))

		if rec2.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
		}
		if receiving.gotCursor == nil {
			t.Fatal("the cursor did not reach the service")
		}
		if receiving.gotCursor.ID != m.ID {
			t.Errorf("cursor id = %v, want %v", receiving.gotCursor.ID, m.ID)
		}
		if !receiving.gotCursor.CreatedAt.Equal(m.CreatedAt) {
			t.Errorf("cursor time = %v, want %v", receiving.gotCursor.CreatedAt, m.CreatedAt)
		}
	})

	t.Run("rejects a tampered cursor", func(t *testing.T) {
		t.Parallel()

		for _, cursor := range []string{"not-base64!!", "YWJj", "eyJ0IjoiIn0"} {
			rec := httptest.NewRecorder()
			newTestRouter(t, &stubService{}).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, "/v1/monitors?cursor="+cursor, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("cursor %q: status = %d, want %d", cursor, got, want)
			}
		}
	})

	t.Run("rejects a bad limit", func(t *testing.T) {
		t.Parallel()

		for _, limit := range []string{"abc", "0", "-1", "101"} {
			rec := httptest.NewRecorder()
			newTestRouter(t, &stubService{}).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, "/v1/monitors?limit="+limit, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("limit %q: status = %d, want %d", limit, got, want)
			}
		}
	})

	t.Run("passes a valid limit through", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, "/v1/monitors?limit=50", nil))

		if svc.gotLimit != 50 {
			t.Errorf("service received limit %d, want 50", svc.gotLimit)
		}
	})
}

// TestRequestIDIsEchoed lets a caller reporting a failure be matched to the log
// record for it.
func TestRequestIDIsEchoed(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := authedRequest(t, http.MethodGet, "/v1/monitors/not-a-uuid", nil)
	req.Header.Set("X-Request-Id", "test-request-id")

	newTestRouter(t, &stubService{}).ServeHTTP(rec, req)

	if p := decodeProblem(t, rec); p.RequestID != "test-request-id" {
		t.Errorf("request_id = %q, want the client-supplied %q", p.RequestID, "test-request-id")
	}
}

// errStorage stands in for an unexpected failure from below the service.
var errStorage = errors.New("connection refused to postgres at 10.0.0.5:5432")
