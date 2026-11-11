package httpapi

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

func TestCheckCursorRoundTrip(t *testing.T) {
	t.Parallel()

	// Microsecond precision is what PostgreSQL stores; nanoseconds survive too.
	want := monitor.CheckCursor{
		CheckedAt: time.Date(2026, time.November, 11, 12, 0, 0, 123456789, time.UTC),
		ID:        9_007_199_254_740_993, // beyond float64's exact integer range
	}

	got, err := decodeCheckCursor(encodeCheckCursor(&want))
	if err != nil {
		t.Fatalf("decodeCheckCursor() returned an unexpected error: %v", err)
	}
	if got.ID != want.ID || !got.CheckedAt.Equal(want.CheckedAt) {
		t.Errorf("round trip = %+v, want %+v", *got, want)
	}

	if encodeCheckCursor(nil) != "" {
		t.Error("a nil cursor did not encode to the empty string")
	}
	if c, err := decodeCheckCursor(""); c != nil || err != nil {
		t.Errorf("empty cursor = %v, %v; want nil, nil", c, err)
	}
}

func TestMonitorCursorRoundTrip(t *testing.T) {
	t.Parallel()

	want := monitor.Cursor{CreatedAt: time.Date(2026, time.August, 17, 12, 0, 0, 1000, time.UTC), ID: uuid.New()}

	got, err := decodeCursor(encodeCursor(&want))
	if err != nil {
		t.Fatalf("decodeCursor() returned an unexpected error: %v", err)
	}
	if got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("round trip = %+v, want %+v", *got, want)
	}
}

func TestCursorKindMismatchIsRejected(t *testing.T) {
	t.Parallel()

	monitorCursor := encodeCursor(&monitor.Cursor{CreatedAt: time.Now(), ID: uuid.New()})
	checkCursor := encodeCheckCursor(&monitor.CheckCursor{CheckedAt: time.Now(), ID: 7})

	if _, err := decodeCheckCursor(monitorCursor); !errors.Is(err, errBadCursor) {
		t.Errorf("monitor cursor decoded as a check cursor: err = %v", err)
	}
	if _, err := decodeCursor(checkCursor); !errors.Is(err, errBadCursor) {
		t.Errorf("check cursor decoded as a monitor cursor: err = %v", err)
	}

	unknown := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"k":"incident","t":"2026-11-11T12:00:00Z","i":7}`))
	if _, err := decodeCheckCursor(unknown); !errors.Is(err, errBadCursor) {
		t.Errorf("unknown kind accepted as a check cursor: err = %v", err)
	}
	if _, err := decodeCursor(unknown); !errors.Is(err, errBadCursor) {
		t.Errorf("unknown kind accepted as a monitor cursor: err = %v", err)
	}
}

// TestLegacyMonitorCursorStillDecodes covers cursors issued before payloads
// carried a kind: a client paging across the deploy must not get a 400.
func TestLegacyMonitorCursorStillDecodes(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	legacy := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"t":"2026-08-17T12:00:00Z","i":"` + id.String() + `"}`))

	got, err := decodeCursor(legacy)
	if err != nil {
		t.Fatalf("legacy monitor cursor rejected: %v", err)
	}
	if got.ID != id {
		t.Errorf("id = %v, want %v", got.ID, id)
	}

	// The same kind-less payload is never a check cursor.
	if _, err := decodeCheckCursor(legacy); !errors.Is(err, errBadCursor) {
		t.Errorf("kind-less cursor accepted as a check cursor: err = %v", err)
	}
}

func TestCheckCursorRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`{"k":"check","t":"2026-11-11T12:00:00Z","i":0}`,
		`{"k":"check","t":"2026-11-11T12:00:00Z","i":-1}`,
		`{"k":"check","i":7}`,
		`{"k":"check","t":"2026-11-11T12:00:00Z","i":"7"}`,
		`not json`,
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
		if _, err := decodeCheckCursor(encoded); !errors.Is(err, errBadCursor) {
			t.Errorf("payload %s: err = %v, want errBadCursor", payload, err)
		}
	}
}
