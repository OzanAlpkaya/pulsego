// Package checker executes outbound HTTP checks over one shared, tuned
// http.Transport, with a per-check deadline taken from the monitor, and
// classifies failures into the domain.ErrorKind values (timeout, dns, redirect,
// tls, connection, unexpected_status, unknown).
//
// Response bodies are drained through a bounded reader and discarded; they are
// never stored or logged.
package checker
