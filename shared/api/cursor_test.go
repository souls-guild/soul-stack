package api

import (
	"encoding/base64"
	"errors"
	"net/url"
	"testing"
	"time"
)

// TestKeysetCursor_RoundTrip — encode→decode возвращает исходные значения
// (composite (registered_at, sid)). registered_at сериализуется в UTC.
func TestKeysetCursor_RoundTrip(t *testing.T) {
	at := time.Date(2026, 5, 20, 15, 29, 55, 0, time.UTC)
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: at, SID: "web-01.example.com"})
	if enc == "" {
		t.Fatal("EncodeKeysetCursor → пустая строка")
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

// TestKeysetCursor_Opaque — кодировка base64url(JSON): курсор непрозрачен, но
// валидно декодируется как base64url. Гарантирует, что клиент не парсит
// внутренности (контракт opaque-курсора).
func TestKeysetCursor_Opaque(t *testing.T) {
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})
	if _, err := base64.RawURLEncoding.DecodeString(enc); err != nil {
		t.Errorf("курсор не base64url-декодируется: %v", err)
	}
}

// TestDecodeKeysetCursor_Bad — битый курсор → ошибка (handler маппит в 400),
// НЕ паника и НЕ молчаливый zero-value.
func TestDecodeKeysetCursor_Bad(t *testing.T) {
	for _, c := range []string{
		"!!!not-base64!!!",
		base64.RawURLEncoding.EncodeToString([]byte("{not-json")),
		base64.RawURLEncoding.EncodeToString([]byte(`{"sid":""}`)), // пустой sid.
		base64.RawURLEncoding.EncodeToString([]byte(`{"registered_at":"bad"}`)),
		// Валидный JSON, непустой sid, но нулевой timestamp → ветка
		// `missing registered_at` (IsZero): курсор по zero-time вернул бы
		// неверную страницу, поэтому отвергаем.
		base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"x","registered_at":"0001-01-01T00:00:00Z"}`)),
	} {
		if _, err := DecodeKeysetCursor(c); err == nil {
			t.Errorf("DecodeKeysetCursor(%q) → nil err; want ошибка", c)
		}
	}
}

// TestParseCursor_FromQuery — извлечение opaque-курсора из query.
func TestParseCursor_FromQuery(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: at, SID: "h.example.com"})
	q := url.Values{"cursor": []string{enc}}
	cur, err := ParseCursor(q)
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	if cur == nil {
		t.Fatal("ParseCursor → nil при заданном cursor")
	}
	if cur.SID != "h.example.com" {
		t.Errorf("SID = %q", cur.SID)
	}
}

// TestParseCursor_Absent — без cursor-param → (nil, nil).
func TestParseCursor_Absent(t *testing.T) {
	cur, err := ParseCursor(url.Values{})
	if err != nil {
		t.Fatalf("ParseCursor empty: %v", err)
	}
	if cur != nil {
		t.Errorf("ParseCursor без cursor → %+v, want nil", cur)
	}
}

// TestParseCursor_Bad — битый cursor → *PaginationError (handler → 400).
func TestParseCursor_Bad(t *testing.T) {
	q := url.Values{"cursor": []string{"!!!bad!!!"}}
	_, err := ParseCursor(q)
	if err == nil {
		t.Fatal("ParseCursor(битый) → nil err")
	}
	var pe *PaginationError
	if !errors.As(err, &pe) {
		t.Errorf("err type = %T, want *PaginationError", err)
	}
}

// TestParsePage_OffsetAndCursorConflict — offset>0 И cursor одновременно →
// ошибка (клиентский баг не маскируется). offset=0 + cursor — ок (offset
// по умолчанию).
func TestParsePage_OffsetAndCursorConflict(t *testing.T) {
	enc := EncodeKeysetCursor(KeysetCursor{RegisteredAt: time.Now().UTC(), SID: "h.example.com"})

	// offset>0 + cursor → конфликт.
	q := url.Values{"offset": []string{"10"}, "cursor": []string{enc}}
	if _, _, err := ParsePageWithCursor(q); err == nil {
		t.Error("offset=10 + cursor → nil err; want конфликт")
	}

	// offset=0 (default) + cursor → ок.
	q2 := url.Values{"cursor": []string{enc}}
	page, cur, err := ParsePageWithCursor(q2)
	if err != nil {
		t.Fatalf("offset=0 + cursor: %v", err)
	}
	if cur == nil || cur.SID != "h.example.com" {
		t.Errorf("cursor не распарсен: %+v", cur)
	}
	if page.Limit != DefaultPageLimit {
		t.Errorf("limit не применился при cursor-режиме: %d", page.Limit)
	}
}
