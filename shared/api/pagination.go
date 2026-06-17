// Package api — общие HTTP-хелперы Operator API (offset/limit pagination
// под operator-api.md § Pagination, и т. п.). Живёт в shared/, чтобы и
// keeper-handler-ы, и MCP-фасад (M0.7+), и будущие push/cloud handler-ы
// читали один и тот же контракт без дублирования парсинга.
package api

import (
	"fmt"
	"net/url"
	"strconv"
)

// Page-defaults / limits под operator-api.md → § Pagination.
const (
	// DefaultPageLimit — limit по умолчанию, если query-param не задан.
	DefaultPageLimit = 50

	// MaxPageLimit — верхняя граница limit-а. Запрос с limit > MaxPageLimit
	// отклоняется как malformed-request (operator-api.md: «1..1000»).
	MaxPageLimit = 1000
)

// Page — нормализованные параметры пагинации. Offset ≥ 0, Limit ∈ [1, MaxPageLimit].
// Конструктор [ParsePage] гарантирует инварианты — внутрь структуру можно
// не валидировать повторно.
type Page struct {
	Offset int
	Limit  int
}

// PagedResponse — общий конверт для list-эндпоинтов:
//
//	{ "items": [...], "offset": 0, "limit": 50, "total": 137 }
//
// Items типизируется на call-site (PagedResponse[IncarnationDTO] и т. п.).
// total — общее количество элементов с учётом фильтров endpoint-а.
//
// Гибрид offset/keyset (ADR-047 S3b-2, additive): один и тот же конверт
// обслуживает оба режима пагинации; режим выбирает СЕРВЕР (не клиент):
//   - offset-режим (SQL-pushdown полон, дрейфа total нет): total — точное
//     число, [PagedResponse.TotalApproximate]=false (поле опущено),
//     [PagedResponse.NextCursor]=nil. Это backward-compatible дефолт прежних
//     list-эндпоинтов — zero-value не сериализуется, wire-форма не меняется.
//   - keyset-режим (Go-постфильтр поверх внутренних страниц — точный COUNT
//     дорог/недоступен): total опускается (0), TotalApproximate=true, NextCursor
//     несёт opaque-курсор следующей страницы (nil ⟺ БД исчерпана).
type PagedResponse[T any] struct {
	Items  []T `json:"items"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`

	// NextCursor — opaque keyset-курсор следующей страницы (keyset-режим).
	// nil/отсутствует в offset-режиме И когда keyset-обход исчерпал БД.
	NextCursor *string `json:"next_cursor,omitempty"`

	// TotalApproximate — Total НЕ точен (keyset-режим: COUNT не считается, поле
	// опущено в 0). Инвертированный флаг с omitempty: zero-value (все offset-
	// эндпоинты) не сериализуется → точный total по умолчанию, без протечки
	// wire-поля на не-keyset-хендлеры. Выставляет только keyset-path souls.
	TotalApproximate bool `json:"total_approximate,omitempty"`
}

// PaginationError — sentinel для ошибок парсинга. Caller (handler) маппит
// в RFC 7807 malformed-request (400) с err.Error() в detail.
//
// conflict=true помечает offset+cursor-конфликт (см. [ParsePageWithCursor]):
// это клиентский баг (смешение двух пагинаций), handler маппит его в 422
// (validation-failed), а не в 400 — отличается через [PaginationError.IsConflict].
type PaginationError struct {
	msg      string
	conflict bool
}

func (e *PaginationError) Error() string { return e.msg }

// IsConflict — это offset+cursor-конфликт (422), а не обычная malformed-ошибка
// парсинга (400)?
func (e *PaginationError) IsConflict() bool { return e.conflict }

// ParsePage парсит offset/limit из url.Values (обычно r.URL.Query()).
//
// Контракт:
//   - offset отсутствует или пуст → 0; иначе должен быть валидное неотрицательное число.
//   - limit  отсутствует или пуст → [DefaultPageLimit]; иначе ∈ [1, MaxPageLimit].
//   - "abc" / отрицательные / превышение MaxPageLimit → *PaginationError.
//
// Ошибки возвращаются как *PaginationError, чтобы handler мог отличить
// pagination-validation от других malformed-сценариев.
func ParsePage(q url.Values) (Page, error) {
	p := Page{Offset: 0, Limit: DefaultPageLimit}

	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Page{}, &PaginationError{msg: fmt.Sprintf("invalid offset %q: must be integer", raw)}
		}
		p.Offset = v
	}

	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Page{}, &PaginationError{msg: fmt.Sprintf("invalid limit %q: must be integer", raw)}
		}
		p.Limit = v
	}

	if err := CheckPageBounds(p.Offset, p.Limit); err != nil {
		return Page{}, err
	}
	return p, nil
}

// CheckPageBounds валидирует ДИАПАЗОН уже-распарсенных offset/limit (offset ≥ 0,
// limit ∈ [1, MaxPageLimit]), возвращая *PaginationError с тем же сообщением, что и
// [ParsePage]. Вынесено отдельной функцией ради ЕДИНОГО источника правды границ:
// typed-query-эндпоинты (ADR-054 четвёртый tier), где int-bind делает huma (а не
// ParsePage из url.Values), всё равно обязаны держать тот же 400-контракт на
// out-of-range (иначе wire-change limit=0/1001/offset<0). Caller (handler) маппит
// ошибку в RFC 7807 malformed-request (400).
func CheckPageBounds(offset, limit int) error {
	if offset < 0 {
		return &PaginationError{msg: fmt.Sprintf("invalid offset %d: must be >= 0", offset)}
	}
	if limit < 1 {
		return &PaginationError{msg: fmt.Sprintf("invalid limit %d: must be >= 1", limit)}
	}
	if limit > MaxPageLimit {
		return &PaginationError{msg: fmt.Sprintf("invalid limit %d: must be <= %d", limit, MaxPageLimit)}
	}
	return nil
}
