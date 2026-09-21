// Package api holds shared Operator API HTTP helpers (offset/limit pagination per
// operator-api.md § Pagination, etc.). Lives in shared/ so keeper handlers, the MCP
// facade (M0.7+), and future push/cloud handlers read one contract without
// duplicating parsing.
package api

import (
	"fmt"
	"net/url"
	"strconv"
)

// Page defaults / limits per operator-api.md → § Pagination.
const (
	// DefaultPageLimit is the default limit when the query param is unset.
	DefaultPageLimit = 50

	// MaxPageLimit is the upper bound on limit. A request with limit > MaxPageLimit
	// is rejected as a malformed-request (operator-api.md: "1..1000").
	MaxPageLimit = 1000
)

// Page holds normalized pagination parameters. Offset ≥ 0, Limit ∈ [1, MaxPageLimit].
// The [ParsePage] constructor guarantees the invariants — no need to re-validate the
// struct internally.
type Page struct {
	Offset int
	Limit  int
}

// PagedResponse is the shared envelope for list endpoints:
//
//	{ "items": [...], "offset": 0, "limit": 50, "total": 137 }
//
// Items is typed at the call site (PagedResponse[IncarnationDTO], etc.). total is the
// total element count under the endpoint's filters.
//
// Offset/keyset hybrid (ADR-047 S3b-2, additive): one envelope serves both pagination
// modes; the SERVER (not the client) picks the mode:
//   - offset mode (full SQL pushdown, no total drift): total is exact,
//     [PagedResponse.TotalApproximate]=false (field omitted),
//     [PagedResponse.NextCursor]=nil. This is the backward-compatible default of the
//     former list endpoints — the zero-value is not serialized, the wire form is unchanged.
//   - keyset mode (Go post-filter over internal pages — an exact COUNT is
//     costly/unavailable): total is omitted (0), TotalApproximate=true, NextCursor
//     carries an opaque cursor to the next page (nil ⟺ DB exhausted).
type PagedResponse[T any] struct {
	Items  []T `json:"items"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`

	// NextCursor is an opaque keyset cursor to the next page (keyset mode).
	// nil/absent in offset mode AND when the keyset walk has exhausted the DB.
	NextCursor *string `json:"next_cursor,omitempty"`

	// TotalApproximate — Total is NOT exact (keyset mode: COUNT is not computed, field
	// omitted to 0). Inverted flag with omitempty: the zero-value (all offset
	// endpoints) is not serialized → exact total by default, without leaking the wire
	// field onto non-keyset handlers. Set only by the keyset path of souls.
	TotalApproximate bool `json:"total_approximate,omitempty"`
}

// PaginationError is a sentinel for parse errors. The caller (handler) maps it to an
// RFC 7807 malformed-request (400) with err.Error() in detail.
//
// conflict=true marks an offset+cursor conflict (see [ParsePageWithCursor]): a client
// bug (mixing two paginations), which the handler maps to 422 (validation-failed),
// not 400 — distinguished via [PaginationError.IsConflict].
type PaginationError struct {
	msg      string
	conflict bool
}

func (e *PaginationError) Error() string { return e.msg }

// IsConflict reports whether this is an offset+cursor conflict (422) rather than an
// ordinary malformed parse error (400).
func (e *PaginationError) IsConflict() bool { return e.conflict }

// ParsePage parses offset/limit from url.Values (usually r.URL.Query()).
//
// Contract:
//   - offset absent or empty → 0; otherwise must be a valid non-negative integer.
//   - limit  absent or empty → [DefaultPageLimit]; otherwise ∈ [1, MaxPageLimit].
//   - "abc" / negative / exceeding MaxPageLimit → *PaginationError.
//
// Errors are returned as *PaginationError so the handler can distinguish
// pagination-validation from other malformed scenarios.
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

// CheckPageBounds validates the RANGE of already-parsed offset/limit (offset ≥ 0,
// limit ∈ [1, MaxPageLimit]), returning a *PaginationError with the same message as
// [ParsePage]. Split into a separate function for a SINGLE source of truth for the
// bounds: typed-query endpoints (ADR-054 fourth tier), where the int-bind is done by
// huma (not ParsePage from url.Values), must still hold the same 400 contract on
// out-of-range (otherwise a wire-change limit=0/1001/offset<0). The caller (handler)
// maps the error to an RFC 7807 malformed-request (400).
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
