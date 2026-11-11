package domain_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/OzanAlpkaya/pulsego/internal/domain"
)

func TestIncidentCause(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }
	const limit = domain.MaxIncidentCauseLength

	tests := []struct {
		name    string
		kind    domain.ErrorKind
		msg     *string
		want    string // checked when wantLen is 0
		wantLen int
	}{
		{name: "kind and message", kind: domain.ErrorKindConnection, msg: str("connection refused"), want: "connection: connection refused"},
		{name: "nil message", kind: domain.ErrorKindTimeout, want: "timeout"},
		{name: "empty message", kind: domain.ErrorKindDNS, msg: str(""), want: "dns"},
		{name: "exactly the limit", kind: domain.ErrorKindUnknown, msg: str(strings.Repeat("x", limit-len("unknown: "))), wantLen: limit},
		{name: "ascii over the limit", kind: domain.ErrorKindUnknown, msg: str(strings.Repeat("x", 2*limit)), wantLen: limit},
		// "tls: " is 5 bytes, then 3-byte runes: 5+3*169 = 512 is a boundary.
		{name: "rune on the boundary", kind: domain.ErrorKindTLS, msg: str(strings.Repeat("€", 200)), wantLen: 5 + 3*169},
		// "dns: " is 5 bytes, then 4-byte runes: the rune spanning byte 512 is dropped.
		{name: "rune across the boundary", kind: domain.ErrorKindDNS, msg: str(strings.Repeat("😀", 200)), wantLen: 5 + 4*126},
		{name: "invalid UTF-8 is repaired", kind: domain.ErrorKindUnknown, msg: str("bad\xffbyte"), want: "unknown: bad�byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := domain.IncidentCause(tt.kind, tt.msg)
			if !utf8.ValidString(got) {
				t.Fatalf("IncidentCause() = %q, not valid UTF-8", got)
			}
			if len(got) > limit {
				t.Fatalf("len = %d, exceeds %d", len(got), limit)
			}
			if tt.wantLen == 0 {
				if got != tt.want {
					t.Errorf("IncidentCause() = %q, want %q", got, tt.want)
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			full := tt.kind.String() + ": " + *tt.msg
			if !strings.HasPrefix(full, got) {
				t.Errorf("result is not a prefix of %q…", full[:20])
			}
		})
	}
}
