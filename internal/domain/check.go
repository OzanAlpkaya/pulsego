package domain

import (
	"time"

	"github.com/google/uuid"
)

// ErrorKind classifies why a check failed. The set of values is mirrored by
// the monitor_checks_error_kind_valid CHECK constraint (created in
// db/migrations/00003_create_monitor_checks.sql, last recreated in
// 00005_error_kind_blocked.sql), so adding one here requires a migration.
type ErrorKind string

const (
	// ErrorKindTimeout means the check did not complete within the monitor's
	// timeout, including a DNS lookup cut off by the deadline.
	ErrorKindTimeout ErrorKind = "timeout"
	// ErrorKindDNS means the target host name could not be resolved.
	ErrorKindDNS ErrorKind = "dns"
	// ErrorKindConnection means the connection was refused, reset, or could
	// not be established.
	ErrorKindConnection ErrorKind = "connection"
	// ErrorKindTLS means the TLS handshake or certificate verification failed.
	ErrorKindTLS ErrorKind = "tls"
	// ErrorKindRedirect means the target exceeded the redirect limit.
	ErrorKindRedirect ErrorKind = "redirect"
	// ErrorKindUnexpectedStatus means a response arrived, but its status code
	// was outside the monitor's expected range.
	ErrorKindUnexpectedStatus ErrorKind = "unexpected_status"
	// ErrorKindBlocked means the SSRF policy (internal/security) refused the
	// target or a redirect hop before any connection was made to it: a
	// private, loopback or otherwise non-public address, a disallowed port,
	// or a URL the policy rejects.
	ErrorKindBlocked ErrorKind = "blocked"
	// ErrorKindUnknown is any failure that fits none of the other kinds.
	ErrorKindUnknown ErrorKind = "unknown"
)

// ErrorKinds lists every valid error kind.
func ErrorKinds() []ErrorKind {
	return []ErrorKind{
		ErrorKindTimeout, ErrorKindDNS, ErrorKindConnection, ErrorKindTLS,
		ErrorKindRedirect, ErrorKindUnexpectedStatus, ErrorKindBlocked, ErrorKindUnknown,
	}
}

// Valid reports whether k is an error kind the database will accept.
func (k ErrorKind) Valid() bool {
	switch k {
	case ErrorKindTimeout, ErrorKindDNS, ErrorKindConnection, ErrorKindTLS,
		ErrorKindRedirect, ErrorKindUnexpectedStatus, ErrorKindBlocked, ErrorKindUnknown:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (k ErrorKind) String() string { return string(k) }

// MaxErrorMessageLength is the longest error message, in bytes, that a check
// stores. It mirrors the monitor_checks_error_message_length constraint.
const MaxErrorMessageLength = 512

// CheckOutcome is what the checker produces for one check of a monitor.
//
// Success is true exactly when ErrorKind is nil, and a successful check always
// has a StatusCode; ErrorMessage is set only on failure. The database enforces
// the same rules.
type CheckOutcome struct {
	Success      bool
	StatusCode   *int32
	LatencyMS    int32
	ErrorKind    *ErrorKind
	ErrorMessage *string
}

// Check is a stored check result: an outcome plus its identity and the time
// PostgreSQL recorded it.
type Check struct {
	CheckOutcome

	ID        int64
	MonitorID uuid.UUID
	CheckedAt time.Time
}
