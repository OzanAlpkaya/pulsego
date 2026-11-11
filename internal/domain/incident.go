package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxIncidentCauseLength is the longest cause, in bytes, that an incident
// stores. It mirrors the incidents_cause_length constraint in
// db/migrations/00004_create_incidents.sql.
const MaxIncidentCauseLength = 512

// Incident is a period during which a monitor was down. It mirrors a row of
// the incidents table.
//
// An incident is open while ResolvedAt is nil; the database allows at most
// one open incident per monitor. StartedAt and ResolvedAt are the checked_at
// of the opening and closing checks. The check references become nil when
// retention deletes those checks, but the incident itself is kept.
type Incident struct {
	ID             uuid.UUID
	MonitorID      uuid.UUID
	StartedAt      time.Time
	ResolvedAt     *time.Time
	OpeningCheckID *int64
	ClosingCheckID *int64
	Cause          *string
	CreatedAt      time.Time
}

// IncidentCause renders the cause stored on an incident from its opening
// check: "<kind>: <message>", or just "<kind>" when there is no message. The
// result is valid UTF-8 of at most MaxIncidentCauseLength bytes. It is built
// only from the classified error, never from a response body.
func IncidentCause(kind ErrorKind, msg *string) string {
	cause := kind.String()
	if msg != nil && *msg != "" {
		cause += ": " + *msg
	}
	return TruncateUTF8(cause, MaxIncidentCauseLength)
}

// TruncateUTF8 returns s as valid UTF-8, cut to at most limit bytes without
// splitting a rune. Invalid byte sequences are replaced with U+FFFD first.
func TruncateUTF8(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
