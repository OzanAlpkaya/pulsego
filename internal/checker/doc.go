// Package checker executes outbound HTTP checks over one shared, tuned
// http.Transport, with a per-check deadline taken from the monitor, and
// classifies failures into the domain.ErrorKind values (blocked, timeout, dns,
// redirect, tls, connection, unexpected_status, unknown).
//
// Every outbound connection goes through internal/security: the monitor URL
// and each redirect hop are validated statically, and the dialer resolves the
// host once and connects only to validated IP addresses.
//
// Response bodies are drained through a bounded reader and discarded; they are
// never stored or logged.
package checker
