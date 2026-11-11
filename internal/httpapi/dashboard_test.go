package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// getDashboard sends an unauthenticated GET to the public router.
func getDashboard(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	newTestRouter(t, &stubService{}).ServeHTTP(rec, req)
	return rec
}

// assertDashboardHeaders checks the security headers every dashboard response
// must carry.
func assertDashboardHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want default-src 'self'", csp)
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("Content-Security-Policy = %q must not relax inline script or eval", csp)
	}
	if got, want := rec.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Referrer-Policy"), "no-referrer"; got != want {
		t.Errorf("Referrer-Policy = %q, want %q", got, want)
	}
}

func TestDashboardIndexNeedsNoAPIKey(t *testing.T) {
	t.Parallel()

	rec := getDashboard(t, "/")

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	assertDashboardHeaders(t, rec)
	if !strings.Contains(rec.Body.String(), `src="/static/app.js"`) {
		t.Error("index.html does not load /static/app.js")
	}
}

func TestDashboardServesStaticFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		wantType string
	}{
		{path: "/static/app.js", wantType: "text/javascript"},
		{path: "/static/style.css", wantType: "text/css"},
		{path: "/static/favicon.svg", wantType: "image/svg+xml"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			rec := getDashboard(t, tt.path)

			if got, want := rec.Code, http.StatusOK; got != want {
				t.Fatalf("status = %d, want %d", got, want)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.wantType) {
				t.Errorf("Content-Type = %q, want %s", got, tt.wantType)
			}
			if rec.Body.Len() == 0 {
				t.Error("body is empty")
			}
			assertDashboardHeaders(t, rec)
		})
	}
}

func TestDashboardMissingFilesAre404(t *testing.T) {
	t.Parallel()

	// Directories, traversal attempts and unknown names all fail the same way,
	// and nothing outside the dashboard falls through to it.
	for _, path := range []string{
		"/static/missing.js",
		"/static/",
		"/static/../embed.go",
		"/static/%2e%2e/embed.go",
		"/static//app.js",
		"/index.html",
		"/app.js",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			if got, want := getDashboard(t, path).Code, http.StatusNotFound; got != want {
				t.Errorf("status = %d, want %d", got, want)
			}
		})
	}
}

func TestDashboardDoesNotOpenTheAPI(t *testing.T) {
	t.Parallel()

	svc := &stubService{}
	rec := httptest.NewRecorder()
	newTestRouter(t, svc).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/monitors", nil))

	if got, want := rec.Code, http.StatusUnauthorized; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if svc.gotLimit != 0 {
		t.Error("the request reached the service without a credential")
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Error("API responses should not carry the dashboard headers")
	}
}

// TestDashboardIndexIsCSPCompatible guards default-src 'self': inline script
// bodies, style attributes, <style> blocks and inline event handlers would all
// be blocked by the browser, so they must not appear in the page.
func TestDashboardIndexIsCSPCompatible(t *testing.T) {
	t.Parallel()

	body := getDashboard(t, "/").Body.String()

	inlineScript := regexp.MustCompile(`(?is)<script\b[^>]*>\s*\S.*?</script>`)
	if m := inlineScript.FindString(body); m != "" {
		t.Errorf("index.html has an inline script: %q", m)
	}
	for _, script := range regexp.MustCompile(`(?is)<script\b[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(script, `src="/static/`) {
			t.Errorf("script tag %q does not load a same-origin static file", script)
		}
	}
	checks := map[string]*regexp.Regexp{
		"style attribute":      regexp.MustCompile(`(?i)\sstyle\s*=`),
		"style element":        regexp.MustCompile(`(?i)<style\b`),
		"inline event handler": regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		"javascript: URL":      regexp.MustCompile(`(?i)javascript:`),
		"external resource":    regexp.MustCompile(`(?i)(src|href)\s*=\s*"(https?:)?//`),
	}
	for name, re := range checks {
		if m := re.FindString(body); m != "" {
			t.Errorf("index.html has a %s: %q", name, m)
		}
	}
}
