package api

import (
	"encoding/base64"
	"errors"
	"net/url"
	"testing"
	"time"
)

// TestKeysetCursor_RoundTrip — encode→decode returns the original values
// (composite (registered_at, sid)). registered_at is serialized as UTC.
func TestKeysetCursor_RoundTrip(t *testing.T) {
	at := time.Date(2026, 5, 20, 15, 29, 55, 0, time.UTC)
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: at, SID: "web-01.example.com"})
	if enc == "" {
		t.Fatal("EncodeKeysetCursor -> empty string")
	}
	got, err := DecodeKeysetCursor(enc)
	if err != nil {
		t.Fatalf("DecodeKeysetCursor: %v", err)
	}
	if !got.RegisteredAt.Equal(at) {
		t.Errorf("RegisteredAt = %v, want %v", got.RegisteredAt, at)
	}
	if got.SID != "web-01.example.com" {
		t.Errorf("SID = %q, want web-01.example.com", got.SID)
	}
}

// TestKeysetCursor_Opaque — base64url(JSON) encoding: the cursor is opaque but
// decodes validly as base64url. Guarantees the client does not parse its
// internals (the opaque-cursor contract).
func TestKeysetCursor_Opaque(t *testing.T) {
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})
	if _, err := base64.RawURLEncoding.DecodeString(enc); err != nil {
		t.Errorf("cursor does not base64url-decode: %v", err)
	}
}

// TestDecodeKeysetCursor_Bad — a malformed cursor → error (handler maps to 400),
// NOT a panic and NOT a silent zero value.
func TestDecodeKeysetCursor_Bad(t *testing.T) {
	for _, c := range []string{
		"!!!not-base64!!!",
		base64.RawURLEncoding.EncodeToString([]byte("{not-json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"sid":""}`)), // empty sid.
		base64.RawURLEncoding.EncodeToString([]byte(`{"registered_at":"bad"}`)),
		// Valid JSON, non-empty sid, but a zero timestamp → the
		// `missing registered_at` branch (IsZero): a zero-time cursor would
		// return the wrong page, so we reject it.
		base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"x","registered_at":"0001-01-01T00:00:00Z"}`)),
	} {
		if _, err := DecodeKeysetCursor(c); err == nil {
			t.Errorf("DecodeKeysetCursor(%q) -> nil err; want error", c)
		}
	}
}

// TestParseCursor_FromQuery — extracts the opaque cursor from the query.
func TestParseCursor_FromQuery(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: at, SID: "h.example.com"})
	q := url.Values{"cursor": []string{enc}}
	cur, err := ParseCursor(q)
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	if cur == nil {
		t.Fatal("ParseCursor -> nil with cursor given")
	}
	if cur.SID != "h.example.com" {
		t.Errorf("SID = %q", cur.SID)
	}
}

// TestParseCursor_Absent — no cursor param → (nil, nil).
func TestParseCursor_Absent(t *testing.T) {
	cur, err := ParseCursor(url.Values{})
	if err != nil {
		t.Fatalf("ParseCursor empty: %v", err)
	}
	if cur != nil {
		t.Errorf("ParseCursor without cursor -> %+v, want nil", cur)
	}
}

// TestParseCursor_Bad — malformed cursor → *PaginationError (handler → 400).
func TestParseCursor_Bad(t *testing.T) {
	q := url.Values{"cursor": []string{"!!!bad!!!"}}
	_, err := ParseCursor(q)
	if err == nil {
		t.Fatal("ParseCursor(malformed) -> nil err")
	}
	var pe *PaginationError
	if !errors.As(err, &pe) {
		t.Errorf("err type = %T, want *PaginationError", err)
	}
}

// TestParsePage_OffsetAndCursorConflict — offset>0 AND cursor together →
// error (a client bug is not masked). offset=0 + cursor is ok (default
// offset).
func TestParsePage_OffsetAndCursorConflict(t *testing.T) {
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})

	// offset>0 + cursor → conflict.
	q := url.Values{"offset": []string{"10"}, "cursor": []string{enc}}
	if _, _, err := ParsePageWithCursor(q); err == nil {
		t.Error("offset=10 + cursor -> nil err; want conflict")
	}

	// offset=0 (default) + cursor → ok.
	q2 := url.Values{"cursor": []string{enc}}
	page, cur, err := ParsePageWithCursor(q2)
	if err != nil {
		t.Fatalf("offset=0 + cursor: %v", err)
	}
	if cur == nil || cur.SID != "h.example.com" {
		t.Errorf("cursor not parsed: %+v", cur)
	}
	if page.Limit != DefaultPageLimit {
		t.Errorf("limit was not applied in cursor mode: %d", page.Limit)
	}
}
