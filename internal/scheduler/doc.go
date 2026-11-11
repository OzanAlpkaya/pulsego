// Package scheduler claims due monitors from PostgreSQL using
// FOR UPDATE SKIP LOCKED, assigns time-bounded leases, and runs their checks
// with bounded concurrency. Leases are what make abandoned work recoverable
// when a worker dies mid-check.
package scheduler
