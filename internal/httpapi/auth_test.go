package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/httpapi"
)

const testAPIKey = "0123456789abcdef0123456789abcdef"

func TestAPIKeyAuthAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
	}{
		{name: "canonical scheme", header: "Bearer " + testAPIKey},
		{name: "lowercase scheme", header: "bearer " + testAPIKey},
		{name: "mixed-case scheme", header: "BeArEr " + testAPIKey},
		{name: "padded token", header: "Bearer   " + testAPIKey + "  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors", nil)
			req.Header.Set("Authorization", tt.header)

			newTestRouter(t, &stubService{}).ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				t.Errorf("status = 401 for a valid key sent as %q", tt.header)
			}
		})
	}
}

func TestAPIKeyAuthRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		set    bool
	}{
		{name: "missing header", set: false},
		{name: "empty header", header: "", set: true},
		{name: "wrong key", header: "Bearer wrong-key-entirely-abcdefgh", set: true},
		{name: "key differing in one character", header: "Bearer 0123456789abcdef0123456789abcdeF", set: true},
		{name: "prefix of the real key", header: "Bearer 0123456789abcdef", set: true},
		{name: "real key plus a suffix", header: "Bearer " + testAPIKey + "x", set: true},
		{name: "no scheme", header: testAPIKey, set: true},
		{name: "wrong scheme", header: "Basic " + testAPIKey, set: true},
		{name: "scheme only", header: "Bearer", set: true},
		{name: "scheme with empty token", header: "Bearer ", set: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors", nil)
			if tt.set {
				req.Header.Set("Authorization", tt.header)
			}

			newTestRouter(t, &stubService{}).ServeHTTP(rec, req)

			if got, want := rec.Code, http.StatusUnauthorized; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			if got, want := rec.Header().Get("Content-Type"), httpapi.ContentTypeProblem; got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("WWW-Authenticate header is missing from a 401")
			}

			var problem httpapi.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("body is not valid JSON: %v (body: %q)", err, rec.Body.String())
			}
			if problem.Type != httpapi.TypeUnauthorized {
				t.Errorf("type = %q, want %q", problem.Type, httpapi.TypeUnauthorized)
			}
			if problem.RequestID == "" {
				t.Error("request_id is empty on a 401")
			}
		})
	}
}

// TestAuthNeverEchoesTheKey guards CLAUDE.md rule 10 at the transport edge: a
// rejection must not reflect the credential back, in the body or a header.
func TestAuthNeverEchoesTheKey(t *testing.T) {
	t.Parallel()

	const presented = "some-secret-value-that-is-wrong"

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors", nil)
	req.Header.Set("Authorization", "Bearer "+presented)

	newTestRouter(t, &stubService{}).ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, presented) {
		t.Errorf("response echoed the presented credential: %s", body)
	}
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, presented) || strings.Contains(v, testAPIKey) {
				t.Errorf("header %s leaked a credential: %q", name, v)
			}
		}
	}
	if body := rec.Body.String(); strings.Contains(body, testAPIKey) {
		t.Errorf("response leaked the configured key: %s", body)
	}
}

// TestHealthEndpointsAreUnauthenticated covers the DoD requirement that probes
// work without a credential.
func TestHealthEndpointsAreUnauthenticated(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)

	newTestRouter(t, &stubService{}).ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Errorf("status = %d, want %d without an Authorization header", got, want)
	}
}
