// Package checker executes outbound HTTP checks with bounded concurrency, a
// shared and tuned http.Transport, per-request timeouts, and classification of
// failures into timeout, DNS, connection, and unexpected-status kinds.
//
// Populated in Sprint 3.
package checker
