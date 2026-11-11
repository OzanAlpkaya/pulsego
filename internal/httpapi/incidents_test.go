package httpapi_test

import (
	"encoding/json"
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

const incidentsPath = "/v1/monitors/11111111-2222-3333-4444-555555555555/incidents"

// sampleIncidents returns an open incident followed by a resolved one whose
// opening check has been deleted by retention.
func sampleIncidents() []domain.Incident {
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	resolved := at.Add(-time.Hour)
	return []domain.Incident{
		{
			ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002"), MonitorID: sampleMonitor().ID,
			StartedAt: at, OpeningCheckID: int64Ptr(42),
			Cause: strPtr("timeout: no response within 5000ms"), CreatedAt: at,
		},
		{
			ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001"), MonitorID: sampleMonitor().ID,
			StartedAt: at.Add(-2 * time.Hour), ResolvedAt: &resolved, ClosingCheckID: int64Ptr(40),
			Cause: strPtr("dns"), CreatedAt: at.Add(-2 * time.Hour),
		},
	}
}

func int64Ptr(v int64) *int64 { return &v }

// issuedCursor performs a GET against path with svc and returns the
// next_cursor it issued, failing the test if there is none.
func issuedCursor(t *testing.T, svc *stubService, path string) string {
	t.Helper()

	rec := httptest.NewRecorder()
	newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, path, nil))

	var body struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.NextCursor == "" {
		t.Fatalf("GET %s issued no cursor: %v (status %d, body: %s)", path, err, rec.Code, rec.Body.String())
	}
	return body.NextCursor
}

func TestListIncidentsEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("returns a page in the documented shape", func(t *testing.T) {
		t.Parallel()

		incidents := sampleIncidents()
		svc := &stubService{
			incidents:     incidents,
			incidentsNext: &monitor.IncidentCursor{StartedAt: incidents[1].StartedAt, ID: incidents[1].ID},
		}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, incidentsPath+"?limit=2", nil))

		if got, want := rec.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d (body: %s)", got, want, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if svc.gotIncidentLimit != 2 {
			t.Errorf("service received limit %d, want 2", svc.gotIncidentLimit)
		}
		if svc.gotIncidentCursor != nil {
			t.Errorf("service received cursor %+v on the first page, want nil", svc.gotIncidentCursor)
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

		wantKeys := []string{
			"id", "monitor_id", "started_at", "resolved_at",
			"opening_check_id", "closing_check_id", "cause", "created_at",
		}
		for _, entry := range body.Data {
			if len(entry) != len(wantKeys) {
				t.Errorf("incident has keys %v, want exactly %v", entry, wantKeys)
			}
			for _, k := range wantKeys {
				if _, ok := entry[k]; !ok {
					t.Errorf("incident is missing %q: %v", k, entry)
				}
			}
		}

		open, resolved := body.Data[0], body.Data[1]
		if open["id"] != incidents[0].ID.String() || open["monitor_id"] != sampleMonitor().ID.String() {
			t.Errorf("ids = %v / %v, want %v / %v", open["id"], open["monitor_id"], incidents[0].ID, sampleMonitor().ID)
		}
		if open["started_at"] != "2026-11-11T12:00:00Z" {
			t.Errorf("started_at = %v, want 2026-11-11T12:00:00Z", open["started_at"])
		}
		if open["resolved_at"] != nil || open["closing_check_id"] != nil {
			t.Errorf("an open incident carries resolution fields: %v", open)
		}
		if open["opening_check_id"] != float64(42) || open["cause"] != "timeout: no response within 5000ms" {
			t.Errorf("open incident = %v, want opening_check_id 42 and its cause", open)
		}
		if resolved["resolved_at"] != "2026-11-11T11:00:00Z" || resolved["closing_check_id"] != float64(40) {
			t.Errorf("resolved incident = %v, want resolved_at and closing_check_id set", resolved)
		}
		if resolved["opening_check_id"] != nil {
			t.Errorf("opening_check_id = %v, want null for a check deleted by retention", resolved["opening_check_id"])
		}

		// The issued cursor is accepted on the next request.
		receiving := &stubService{}
		rec2 := httptest.NewRecorder()
		newTestRouter(t, receiving).ServeHTTP(rec2,
			authedRequest(t, http.MethodGet, incidentsPath+"?cursor="+body.NextCursor, nil))

		if rec2.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
		}
		c := receiving.gotIncidentCursor
		if c == nil || c.ID != incidents[1].ID || !c.StartedAt.Equal(incidents[1].StartedAt) {
			t.Errorf("service received cursor %+v, want the issued one", c)
		}
	})

	t.Run("no incidents is an empty page", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		newTestRouter(t, &stubService{}).ServeHTTP(rec, authedRequest(t, http.MethodGet, incidentsPath, nil))

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
			authedRequest(t, http.MethodGet, incidentsPath, nil))

		if got, want := rec.Code, http.StatusNotFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if p := decodeProblem(t, rec); p.Type != httpapi.TypeNotFound {
			t.Errorf("type = %q, want %q", p.Type, httpapi.TypeNotFound)
		}
	})

	t.Run("malformed id is 400", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec,
			authedRequest(t, http.MethodGet, "/v1/monitors/not-a-uuid/incidents", nil))

		if got, want := rec.Code, http.StatusBadRequest; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		if svc.gotIncidentLimit != 0 {
			t.Error("the request reached the service despite a malformed id")
		}
	})

	t.Run("monitor and check cursors are rejected", func(t *testing.T) {
		t.Parallel()

		m := sampleMonitor()
		checks := sampleChecks()
		foreign := map[string]string{
			"monitor": issuedCursor(t,
				&stubService{page: []domain.Monitor{m}, next: &monitor.Cursor{CreatedAt: m.CreatedAt, ID: m.ID}},
				"/v1/monitors"),
			"check": issuedCursor(t,
				&stubService{checks: checks, checksNext: &monitor.CheckCursor{CheckedAt: checks[0].CheckedAt, ID: 42}},
				checksPath),
		}

		for kind, cursor := range foreign {
			svc := &stubService{}
			rec := httptest.NewRecorder()
			newTestRouter(t, svc).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, incidentsPath+"?cursor="+cursor, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("%s cursor: status = %d, want %d", kind, got, want)
				continue
			}
			if p := decodeProblem(t, rec); p.Type != httpapi.TypeMalformed {
				t.Errorf("%s cursor: type = %q, want %q", kind, p.Type, httpapi.TypeMalformed)
			}
			if svc.gotIncidentLimit != 0 {
				t.Errorf("%s cursor: the request reached the service", kind)
			}
		}
	})

	t.Run("an incident cursor is rejected by the monitor and check lists", func(t *testing.T) {
		t.Parallel()

		incidents := sampleIncidents()
		cursor := issuedCursor(t, &stubService{
			incidents:     incidents,
			incidentsNext: &monitor.IncidentCursor{StartedAt: incidents[0].StartedAt, ID: incidents[0].ID},
		}, incidentsPath)

		for _, path := range []string{"/v1/monitors", checksPath} {
			svc := &stubService{}
			rec := httptest.NewRecorder()
			newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, path+"?cursor="+cursor, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("%s: status = %d, want %d", path, got, want)
			}
			if svc.gotLimit != 0 || svc.gotCheckLimit != 0 {
				t.Errorf("%s: the request reached the service despite a wrong-kind cursor", path)
			}
		}
	})

	t.Run("rejects a bad limit or tampered cursor", func(t *testing.T) {
		t.Parallel()

		for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=abc", "cursor=not-base64!!", "cursor=YWJj"} {
			svc := &stubService{}
			rec := httptest.NewRecorder()
			newTestRouter(t, svc).ServeHTTP(rec,
				authedRequest(t, http.MethodGet, incidentsPath+"?"+query, nil))

			if got, want := rec.Code, http.StatusBadRequest; got != want {
				t.Errorf("%s: status = %d, want %d", query, got, want)
			}
			if svc.gotIncidentLimit != 0 {
				t.Errorf("%s: the request reached the service", query)
			}
		}
	})

	t.Run("limit at the maximum is accepted", func(t *testing.T) {
		t.Parallel()

		svc := &stubService{}
		rec := httptest.NewRecorder()
		newTestRouter(t, svc).ServeHTTP(rec, authedRequest(t, http.MethodGet, incidentsPath+"?limit=100", nil))

		if rec.Code != http.StatusOK || svc.gotIncidentLimit != 100 {
			t.Errorf("status = %d, limit = %d; want 200 and 100", rec.Code, svc.gotIncidentLimit)
		}
	})
}
