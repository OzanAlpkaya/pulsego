package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// APIKeyAuth guards a route group with a bearer token.
//
// The comparison is over SHA-256 digests rather than the raw strings.
// subtle.ConstantTimeCompare is only constant-time for equal-length inputs — it
// returns early when the lengths differ — so comparing raw keys would leak the
// length of the configured key. Hashing first makes both sides 32 bytes
// whatever the caller sends.
func APIKeyAuth(apiKey string, log *slog.Logger) func(http.Handler) http.Handler {
	expected := sha256.Sum256([]byte(apiKey))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r)
			if !ok {
				unauthorized(w, r, log, "A bearer token is required. Send: Authorization: Bearer <api-key>")
				return
			}

			actual := sha256.Sum256([]byte(presented))
			if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
				unauthorized(w, r, log, "The supplied API key is not valid.")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the credential from an Authorization header. The scheme
// is matched case-insensitively, as RFC 7235 requires; the token is not.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// unauthorized answers a rejected request. The response never distinguishes a
// missing key from a wrong one in its status, and never echoes what was sent.
func unauthorized(w http.ResponseWriter, r *http.Request, log *slog.Logger, detail string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="pulsego"`)

	writeProblem(w, r, log, Problem{
		Type:   TypeUnauthorized,
		Title:  "Unauthorized",
		Status: http.StatusUnauthorized,
		Detail: detail,
	})
}
