package audit

import (
	"crypto/rand"
	"regexp"

	"github.com/oklog/ulid/v2"
)

// NewULID returns a fresh ULID (26 chars, lexically sortable timestamp prefix +
// random). Used for `audit_id` and also reused by
// shared/config/store.go::newCorrelationID().
//
// `ulid.Make()` uses `entropy.New(rand.Reader, ...)` internally; a reader failure
// means the machine is in a broken state. We keep the same contract as the
// previous `newCorrelationID()` over `crypto/rand`: panic on I/O failure, to
// preserve the string-format invariant instead of silent corruption.
func NewULID() string {
	id, err := ulid.New(ulid.Now(), ulid.Monotonic(rand.Reader, 0))
	if err != nil {
		panic("audit: ulid generation failed: " + err.Error())
	}
	return id.String()
}

// ulidPattern — Crockford base32 (without I, L, O, U). 26 chars. The same regex as
// in shared/audit/ulid_test.go::ulidPattern and in the M0.3 CorrelationID
// regression tests.
var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// IsValidULID does a syntactic check of a ULID string against Crockford base32
// (length 26, chars from `0-9A-HJKMNP-TV-Z`). Used by query-param validation at
// the API boundary (e.g. the `apply_id` filter in
// `/v1/incarnations/{name}/history`) to reject junk before a round-trip to
// Postgres.
func IsValidULID(s string) bool {
	return ulidPattern.MatchString(s)
}
