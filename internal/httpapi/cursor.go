package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/OzanAlpkaya/pulsego/internal/monitor"
)

// errBadCursor is returned for anything that is not a cursor this server
// issued for the endpoint it was sent to. Callers get a 400 rather than a
// silently reset or silently wrong page.
var errBadCursor = errors.New("invalid cursor")

// Cursor kinds. Every payload names the list it belongs to, so a cursor from
// one endpoint sent to another is rejected instead of being reinterpreted.
const (
	cursorKindMonitor = "monitor"
	cursorKindCheck   = "check"
)

// cursorPayload is the wire form of a monitor-list keyset position. It is
// base64url-encoded so that clients treat it as opaque and do not build their
// own.
//
// Kind was added after monitor cursors were first issued. A monitor cursor
// without it is still accepted, so a client paging across a deploy is not
// broken; every other kind must be present and must match.
type cursorPayload struct {
	Kind      string    `json:"k,omitempty"`
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// checkCursorPayload is the wire form of a check-history keyset position.
type checkCursorPayload struct {
	Kind      string    `json:"k"`
	CheckedAt time.Time `json:"t"`
	ID        int64     `json:"i"`
}

// encodeCursor renders a monitor cursor for the next-page link. A nil cursor
// means there is no next page and encodes to the empty string.
func encodeCursor(c *monitor.Cursor) string {
	if c == nil {
		return ""
	}
	return encodePayload(cursorPayload{Kind: cursorKindMonitor, CreatedAt: c.CreatedAt, ID: c.ID})
}

// decodeCursor parses a monitor cursor supplied by a client. An empty string
// is not an error: it means the first page.
func decodeCursor(encoded string) (*monitor.Cursor, error) {
	if encoded == "" {
		return nil, nil
	}

	kind, raw, err := decodeKind(encoded)
	if err != nil {
		return nil, err
	}
	if kind != "" && kind != cursorKindMonitor {
		return nil, errBadCursor
	}

	var payload cursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errBadCursor
	}
	if payload.CreatedAt.IsZero() || payload.ID == uuid.Nil {
		return nil, errBadCursor
	}

	return &monitor.Cursor{CreatedAt: payload.CreatedAt, ID: payload.ID}, nil
}

// encodeCheckCursor renders a check-history cursor for the next-page link. A
// nil cursor encodes to the empty string.
func encodeCheckCursor(c *monitor.CheckCursor) string {
	if c == nil {
		return ""
	}
	return encodePayload(checkCursorPayload{Kind: cursorKindCheck, CheckedAt: c.CheckedAt, ID: c.ID})
}

// decodeCheckCursor parses a check-history cursor supplied by a client. An
// empty string means the first page. Unlike monitor cursors, the kind is
// mandatory: check cursors have always carried it.
func decodeCheckCursor(encoded string) (*monitor.CheckCursor, error) {
	if encoded == "" {
		return nil, nil
	}

	kind, raw, err := decodeKind(encoded)
	if err != nil {
		return nil, err
	}
	if kind != cursorKindCheck {
		return nil, errBadCursor
	}

	var payload checkCursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errBadCursor
	}
	if payload.CheckedAt.IsZero() || payload.ID <= 0 {
		return nil, errBadCursor
	}

	return &monitor.CheckCursor{CheckedAt: payload.CheckedAt, ID: payload.ID}, nil
}

// encodePayload marshals a cursor payload and base64url-encodes it.
func encodePayload(payload any) string {
	// Marshalling a fixed struct of a string, a time and an id cannot fail.
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeKind base64url-decodes a cursor and reads only its kind, returning the
// raw JSON for the caller to decode into the payload type the kind implies.
func decodeKind(encoded string) (string, []byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", nil, errBadCursor
	}

	var header struct {
		Kind string `json:"k"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return "", nil, errBadCursor
	}
	return header.Kind, raw, nil
}
