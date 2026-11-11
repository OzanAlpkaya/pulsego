// Package security validates user-supplied monitoring targets before any
// outbound request is made: scheme and port allowlists, rejection of loopback,
// private, link-local, multicast, unspecified, and IPv6 ULA addresses, a
// TOCTOU-safe dialer that connects to the address that was validated, and
// revalidation of every redirect hop.
//
//   - Policy.ValidateURL is a static check (no DNS), for the API and as a
//     pre-flight check in the checker.
//   - BlockedReason classifies a single address.
//   - Dialer resolves a host once, rejects it if any answer is blocked, and
//     dials only the validated IP literals; the default inner net.Dialer
//     re-checks the socket address in its Control hook.
//   - RedirectPolicy limits redirects (ErrTooManyRedirects) and validates
//     every hop.
//
// Every rejection wraps ErrBlockedTarget. Error messages carry the reason and
// the host only, never the URL's path, query or userinfo.
//
// Populated in Sprint 5. Once it exists it must never be weakened.
package security
