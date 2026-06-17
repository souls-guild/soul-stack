package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// KeysetCursor — composite keyset-курсор `(registered_at, sid)` для
// keyset-пагинации list-эндпоинтов (ADR-047 S3b-2). Голый `sid` дал бы дыры
// на равных `registered_at` (одинаковый таймстемп у пачки хостов), поэтому
// курсор несёт ОБА поля — пара уникальна и устойчива.
//
// Курсор указывает на ПОСЛЕДНИЙ отданный клиенту элемент (после постфильтра),
// а не на последний просмотренный из БД: keyset-окно следующей страницы
// стартует строго за ним.
type KeysetCursor struct {
	RegisteredAt time.Time `json:"registered_at"`
	SID          string    `json:"sid"`
}

// EncodeKeysetCursor сериализует курсор в opaque base64url(JSON). RegisteredAt
// приводится к UTC (стабильная wire-форма, независимая от локали процесса).
func EncodeKeysetCursor(c KeysetCursor) string {
	c.RegisteredAt = c.RegisteredAt.UTC()
	raw, _ := json.Marshal(c) // marshal KeysetCursor не может вернуть ошибку.
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeKeysetCursor разбирает opaque-курсор. Валидирует структуру: битый
// base64 / битый JSON / пустой sid / невалидный timestamp → ошибка (caller
// маппит в 400). Не возвращает молчаливый zero-value на повреждённом входе —
// keyset-окно по zero-курсору вернуло бы клиенту неверную страницу.
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

// ParseCursor извлекает opaque-курсор из query (`cursor=`). Отсутствие
// параметра → (nil, nil); битый курсор → *PaginationError (handler → 400).
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

// ParsePageWithCursor парсит offset/limit ([ParsePage]) и opaque-курсор
// ([ParseCursor]) вместе, отвергая их одновременное задание.
//
// Гибрид-контракт (ADR-047 S3b-2): offset/limit-режим и keyset-cursor-режим
// взаимоисключающи на уровне ОДНОГО запроса. `offset > 0` И `cursor`
// одновременно → *PaginationError (422 у handler-а): это клиентский баг
// (смешение двух пагинаций), маскировать его молча нельзя. `limit` валиден в
// обоих режимах.
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
