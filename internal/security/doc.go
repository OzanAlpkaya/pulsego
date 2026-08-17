// Package security validates user-supplied monitoring targets before any
// outbound request is made: scheme and port allowlists, rejection of loopback,
// private, link-local, multicast, unspecified, and IPv6 ULA addresses, a
// TOCTOU-safe dialer that connects to the address that was validated, and
// revalidation of every redirect hop.
//
// Populated in Sprint 5. Once it exists it must never be weakened.
package security
