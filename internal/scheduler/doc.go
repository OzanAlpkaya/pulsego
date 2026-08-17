// Package scheduler claims due monitors from PostgreSQL using
// FOR UPDATE SKIP LOCKED, assigns time-bounded leases, and hands batches to the
// checker. Leases are what make abandoned work recoverable when a worker dies
// mid-check.
//
// Populated in Sprint 3.
package scheduler
