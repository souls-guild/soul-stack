package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// KeysetCursor is a composite keyset cursor `(registered_at, sid)` for keyset pagination
// of list endpoints (ADR-047 S3b-2). A bare `sid` would leave gaps on equal
// `registered_at` (a batch of hosts with the same timestamp), so the cursor carries BOTH
// fields — the pair is unique and stable.
//
// The cursor points at the LAST element handed to the client (after post-filtering), not
// the last one read from the DB: the next page's keyset window starts strictly after it.
type KeysetCursor struct {
	RegisteredAt time.Time `json:"registered_at"`
	SID          string    `json:"sid"`
}

// EncodeKeysetCursor serializes the cursor to opaque base64url(JSON). RegisteredAt is
// normalized to UTC (a stable wire form, independent of the process locale).
func EncodeKeysetCursor(c KeysetCursor) string {
	c.RegisteredAt = c.RegisteredAt.UTC()
	raw, _ := json.Marshal(c) // marshal of KeysetCursor cannot return an error.
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeKeysetCursor parses an opaque cursor. Validates the structure: broken base64 /
// broken JSON / empty sid / invalid timestamp → error (the caller maps it to 400). Does
// not silently return a zero-value on corrupted input — a zero cursor's keyset window
// would hand the client the wrong page.
func DecodeKeysetCursor(s string) (KeysetCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return KeysetCursor{}, fmt.Errorf("invalid cursor: not base64url: %w", err)
	}
	var c KeysetCursor
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return KeysetCursor{}, fmt.Errorf("invalid cursor: malformed payload: %w", err)
	}
	if c.SID == "" {
		return KeysetCursor{}, fmt.Errorf("invalid cursor: empty sid")
	}
	if c.RegisteredAt.IsZero() {
		return KeysetCursor{}, fmt.Errorf("invalid cursor: missing registered_at")
	}
	return c, nil
}

// ParseCursor extracts the opaque cursor from the query (`cursor=`). An absent parameter
// → (nil, nil); a broken cursor → *PaginationError (handler → 400).
func ParseCursor(q url.Values) (*KeysetCursor, error) {
	raw := q.Get("cursor")
	if raw == "" {
		return nil, nil
	}
	c, err := DecodeKeysetCursor(raw)
	if err != nil {
		return nil, &PaginationError{msg: err.Error()}
	}
	return &c, nil
}

// ParsePageWithCursor parses offset/limit ([ParsePage]) and the opaque cursor
// ([ParseCursor]) together, rejecting them being set at the same time.
//
// Hybrid contract (ADR-047 S3b-2): offset/limit mode and keyset-cursor mode are mutually
// exclusive within ONE request. `offset > 0` AND `cursor` together → *PaginationError
// (422 at the handler): a client bug (mixing two paginations) that must not be masked
// silently. `limit` is valid in both modes.
func ParsePageWithCursor(q url.Values) (Page, *KeysetCursor, error) {
	page, err := ParsePage(q)
	if err != nil {
		return Page{}, nil, err
	}
	cursor, err := ParseCursor(q)
	if err != nil {
		return Page{}, nil, err
	}
	if cursor != nil && page.Offset > 0 {
		return Page{}, nil, &PaginationError{
			msg:      "cursor and offset are mutually exclusive: use either keyset cursor or offset pagination, not both",
			conflict: true,
		}
	}
	return page, cursor, nil
}
