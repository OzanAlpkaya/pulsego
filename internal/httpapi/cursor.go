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
// issued. Callers get a 400 rather than a silently reset page.
var errBadCursor = errors.New("invalid cursor")

// cursorPayload is the wire form of a keyset position. It is base64url-encoded
// so that clients treat it as opaque and do not build their own.
type cursorPayload struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// encodeCursor renders a cursor for the next-page link. A nil cursor means
// there is no next page and encodes to the empty string.
func encodeCursor(c *monitor.Cursor) string {
	if c == nil {
		return ""
	}

	// Marshalling a fixed struct of a time and a UUID cannot fail.
	raw, err := json.Marshal(cursorPayload{CreatedAt: c.CreatedAt, ID: c.ID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor parses a cursor supplied by a client. An empty string is not an
// error: it means the first page.
func decodeCursor(encoded string) (*monitor.Cursor, error) {
	if encoded == "" {
		return nil, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
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
