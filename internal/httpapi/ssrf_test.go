package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
	"github.com/OzanAlpkaya/pulsego/internal/monitor"
	"github.com/OzanAlpkaya/pulsego/internal/security"
)

// recordingStore is the part of monitor.Store that create and update touch.
// The embedded nil interface makes any other call panic, which would show up
// as a test failure rather than silently passing.
type recordingStore struct {
	monitor.Store
	writes int
}

func (s *recordingStore) Create(_ context.Context, p monitor.CreateParams) (domain.Monitor, error) {
	s.writes++
	m := sampleMonitor()
	m.URL = p.URL
	return m, nil
}

func (s *recordingStore) Get(_ context.Context, _ uuid.UUID) (domain.Monitor, error) {
	return sampleMonitor(), nil
}

func (s *recordingStore) Update(_ context.Context, _ monitor.UpdateParams) (domain.Monitor, error) {
	s.writes++
	return sampleMonitor(), nil
}

// TestCreateMonitorRejectsPrivateTarget drives the real monitor service
// behind the router: with the production policy, a private or loopback url
// is a 422 on field url and nothing is stored, for create and update alike.
func TestCreateMonitorRejectsPrivateTarget(t *testing.T) {
	t.Parallel()

	targets := []string{
		"http://127.0.0.1/health",
		"http://localhost/",
		"http://169.254.169.254/latest/meta-data/?token=secret",
		"http://10.0.0.1/",
		"http://[::ffff:127.0.0.1]/",
		"https://example.com:8443/",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			store := &recordingStore{}
			router := newTestRouter(t, monitor.NewService(store,
				monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}, security.Policy{}))

			requests := map[string]*http.Request{
				"create": authedRequest(t, http.MethodPost, "/v1/monitors",
					map[string]any{"name": "internal", "url": target}),
				"update": authedRequest(t, http.MethodPatch, "/v1/monitors/"+sampleMonitor().ID.String(),
					map[string]any{"url": target}),
			}
			for op, req := range requests {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: status = %d, want 422 (body %s)", op, rec.Code, rec.Body)
				}
				if strings.Contains(rec.Body.String(), "secret") {
					t.Errorf("%s: body echoes the query string: %s", op, rec.Body)
				}
				p := decodeProblem(t, rec)
				if p.Type != httpapi.TypeValidation || len(p.Errors) != 1 || p.Errors[0].Field != "url" {
					t.Fatalf("%s: problem = %+v, want one validation error on url", op, p)
				}
			}
			if store.writes != 0 {
				t.Fatalf("store received %d writes, want 0", store.writes)
			}
		})
	}

	// Control: the development policy accepts the same kind of target.
	store := &recordingStore{}
	router := newTestRouter(t, monitor.NewService(store,
		monitor.Defaults{IntervalSeconds: 60, TimeoutMS: 5000}, security.Policy{AllowPrivate: true}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, http.MethodPost, "/v1/monitors",
		map[string]any{"name": "local", "url": "http://127.0.0.1:8081/health"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("AllowPrivate create: status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
}
