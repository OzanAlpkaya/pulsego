package httpapi

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// dashboardCSP allows nothing but same-origin resources: no inline script or
// style, no third-party hosts, no framing, and no native form submission (the
// API key form is handled in JavaScript and must never be sent as a URL).
const dashboardCSP = "default-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// dashboardHeaders sets the security headers every dashboard response carries,
// including its 404s.
func dashboardHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", dashboardCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The files are not fingerprinted, so a browser must revalidate them;
		// otherwise a deploy would leave it running stale JavaScript.
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// dashboardIndex serves index.html for GET /.
func dashboardIndex(static fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, static, "index.html")
	}
}

// dashboardStatic serves GET /static/*. It does not use http.FileServer:
// that lists directories and redirects index.html requests, neither of which
// the dashboard needs.
func dashboardStatic(static fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if name == "" || path.Clean(name) != name || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		serveStatic(w, r, static, name)
	}
}

// serveStatic writes one regular file from static, with its Content-Type
// derived from the extension. Anything else, including a directory, is a 404.
func serveStatic(w http.ResponseWriter, r *http.Request, static fs.FS, name string) {
	data, err := fs.ReadFile(static, name)
	if err != nil {
		// The files are embedded, so a read can only fail because the name
		// does not exist or is a directory.
		http.NotFound(w, r)
		return
	}
	// Embedded files have no modification time; ServeContent then omits
	// Last-Modified and picks the type from the name.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
