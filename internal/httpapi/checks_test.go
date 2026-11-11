package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

const checksPath = "/v1/monitors/11111111-2222-3333-4444-555555555555/checks"

func sampleChecks() []domain.Check {
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	kind := domain.ErrorKindUnexpectedStatus
	return []domain.Check{
		{
			ID: 42, MonitorID: sampleMonitor().ID, CheckedAt: at,
			CheckOutcome: domain.CheckOutcome{Success: true, StatusCode: int32Ptr(200), LatencyMS: 87},
		},
		{
			ID: 41, MonitorID: sampleMonitor().ID, CheckedAt: at.Add(-time.Minute),
			CheckOutcome: domain.CheckOutcome{
				Success: false, StatusCode: int32Ptr(503), LatencyMS: 12,
				ErrorKind: &kind, ErrorMessage: strPtr("status 503 outside expected range 200-399"),
			},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

func TestRequestCheckEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("accepted returns the monitor", func(t *testing.T) {
		t.Parallel()

		m := sampleMonitor()
		svc := &stubService{monitor: m}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodPost, checksPath, nil))

		if got, want := rec.Code, http.StatusAccepted; got != want {
			t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
		}
		if svc.requestedID != m.ID {
			t.Errorf("service received %v, want %v", svc.requestedID, m.ID)
		}

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if body["id"] != m.ID.String() {
			t.Errorf("id = %v, want %v", body["id"], m.ID)
		}
		if _, ok := body["lease_owner"]; ok {
			t.Error("the response exposes lease_owner")
		}
	})

	t.Run("missing monitor is 404", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{err: domain.ErrNotFound}).ServeHTTP(rec,
			authedRequest(t, http.MethodPost, checksPath, nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if p := decodeProblem(t, rec); p.Type != httpapi.TypeNotFound {
			t.Errorf("type = %q, want %q", p.Type, httpapi.TypeNotFound)
		}
	})

	t.Run("disabled monitor is 409", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{err: domain.ErrMonitorDisabled}).ServeHTTP(rec,
			authedRequest(t, http.MethodPost, checksPath, nil))

		if got, want := rec.Code, http.StatusConflict; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		p := decodeProblem(t, rec)
		if p.Type != "urn:pulsego:problem:monitor-disabled" {
			t.Errorf("type = %q, want urn:pulsego:problem:monitor-disabled", p.Type)
		}
	})

	t.Run("malformed id is 400", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec,
			authedRequest(t, http.MethodPost, "/v1/monitors/not-a-uuid/checks", nil))

		if got, want := rec.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})
}

func TestListChecksEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("returns a page in the documented shape", func(t *testing.T) {
		t.Parallel()

		checks := sampleChecks()
		svc := &stubService{
			checks:     checks,
			checksNext: &monitor.CheckCursor{CheckedAt: checks[1].CheckedAt, ID: checks[1].ID},
		}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, checksPath+"?limit=2", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
		}
		if svc.gotCheckLimit != 2 {
			t.Errorf("service received limit %d, want 2", svc.gotCheckLimit)
		}

		var body struct {
			Data       []map[string]any `json:"data"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if len(body.Data) != 2 {
			t.Fatalf("data has %d entries, want 2", len(body.Data))
		}
		if body.NextCursor == "" {
			t.Error("next_cursor is empty despite the service returning one")
		}

		wantKeys := []string{"id", "success", "status_code", "latency_ms", "error_kind", "error_message", "checked_at"}
		for _, entry := range body.Data {
			if len(entry) != len(wantKeys) {
				t.Errorf("check has keys %v, want exactly %v", entry, wantKeys)
			}
			for _, k := range wantKeys {
				if _, ok := entry[k]; !ok {
					t.Errorf("check is missing %q: %v", k, entry)
				}
			}
		}
		if body.Data[0]["error_kind"] != nil || body.Data[0]["error_message"] != nil {
			t.Errorf("a successful check carries an error: %v", body.Data[0])
		}
		if body.Data[1]["error_kind"] != "unexpected_status" {
			t.Errorf("error_kind = %v, want unexpected_status", body.Data[1]["error_kind"])
		}
		if body.Data[0]["id"] != float64(42) {
			t.Errorf("id = %v, want 42", body.Data[0]["id"])
		}

		// The issued cursor is accepted on the next request.
		receiving := &stubService{}
		rec2 := httptest.NewRecorder()
		newTestRouter(t, receiving).ServeHTTP(rec2,
			authedRequest(t, http.MethodGet, checksPath+"?cursor="+body.NextCursor, nil))

		if rec2.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
		}
		if c := receiving.gotCheckCursor; c == nil || c.ID != 41 || !c.CheckedAt.Equal(checks[1].CheckedAt) {
			t.Errorf("service received cursor %+v, want the issued one", c)
		}
	})

	t.Run("no checks is an empty page", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{}).ServeHTTP(rec, authedRequest(t, http.MethodGet, checksPath, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"data":[]`) {
			t.Errorf("empty page did not serialise as []: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "next_cursor") {
			t.Errorf("next_cursor is present on the last page: %s", rec.Body.String())
		}
	})

	t.Run("missing monitor is 404", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{err: domain.ErrNotFound}).ServeHTTP(rec,
			authedRequest(t, http.MethodGet, checksPath, nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		decodeProblem(t, rec)
	})

	t.Run("a monitor cursor is rejected", func(t *testing.T) {
		t.Parallel()

		// Obtain a genuine monitor-list cursor from the monitor endpoint.
		m := sampleMonitor()
		issuing := &stubService{page: []domain.Monitor{m}, next: &monitor.Cursor{CreatedAt: m.CreatedAt, ID: m.ID}}
		rec := httptest.NewRecorder()
		newTestRouter(t, issuing).ServeHTTP(rec, authedRequest(t, http.MethodGet, "/v1/monitors", nil))

		var body struct {
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.NextCursor == "" {
			t.Fatalf("no monitor cursor issued: %v (body: %s)", err, rec.Body.String())
		}

		svc := &stubService{}
		rec2 := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec2,
			authedRequest(t, http.MethodGet, checksPath+"?cursor="+body.NextCursor, nil))

		if got, want := rec2.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if p := decodeProblem(t, rec2); p.Type != httpapi.TypeMalformed {
			t.Errorf("type = %q, want %q", p.Type, httpapi.TypeMalformed)
		}
		if svc.gotCheckLimit != 0 {
			t.Error("the request reached the service despite a wrong-kind cursor")
		}
	})

	t.Run("a check cursor is rejected by the monitor list", func(t *testing.T) {
		t.Parallel()

		checks := sampleChecks()
		issuing := &stubService{checks: checks, checksNext: &monitor.CheckCursor{CheckedAt: checks[0].CheckedAt, ID: 42}}
		rec := httptest.NewRecorder()
		newTestRouter(t, issuing).ServeHTTP(rec, authedRequest(t, http.MethodGet, checksPath, nil))

		var body struct {
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.NextCursor == "" {
			t.Fatalf("no check cursor issued: %v (body: %s)", err, rec.Body.String())
		}

		rec2 := httptest.NewRecorder()
		newTestRouter(t, &stubService{}).ServeHTTP(rec2,
			authedRequest(t, http.MethodGet, "/v1/monitors?cursor="+body.NextCursor, nil))

		if got, want := rec2.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
	})

	t.Run("rejects a bad limit or tampered cursor", func(t *testing.T) {
		t.Parallel()

		for _, query := range []string{"limit=0", "limit=101", "limit=abc", "cursor=not-base64!!", "cursor=YWJj"} {
			rec := httptest.NewRecorder()
			newTestRouter(t, &stubService{}).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, checksPath+"?"+query, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("%s: status = %d, want %d", query, got, want)
			}
		}
	})
}
