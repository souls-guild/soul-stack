package voyage

import (
	"errors"
	"strconv"
	"strings"
)

// BatchSpecMode — discriminator for the parsed string batch field: the batch
// size is given either as an absolute number of hosts/incarnations or as a percentage of scope.
type BatchSpecMode int

const (
	// BatchSpecHosts — `batch: "N"`: absolute Leg size (parity batch_size).
	BatchSpecHosts BatchSpecMode = iota
	// BatchSpecPercent — `batch: "N%"`: percentage of the resolved scope (parity
	// batch_percent), the effective size is computed AFTER resolving scope via
	// the existing effectiveBatchSize.
	BatchSpecPercent
)

// Sentinel errors for [ParseBatchSpec] — the caller maps them to a human-readable 422 detail
// (or treats [ErrBatchSpecEmpty] as "field not set").
//   - ErrBatchSpecEmpty        — empty/whitespace string; not an "input error" but
//     an absent value (caller: the whole scope as one Leg).
//   - ErrBatchSpecMalformed    — doesn't match `^\d+%?$` (dot, sign, garbage,
//     internal whitespace) or the number doesn't fit in int (overflow).
//   - ErrBatchSpecPercentRange — percent outside [1, 100].
//   - ErrBatchSpecHostsRange   — hosts < 1.
var (
	ErrBatchSpecEmpty        = errors.New("voyage: batch spec is empty")
	ErrBatchSpecMalformed    = errors.New("voyage: batch spec malformed (expected N or N%)")
	ErrBatchSpecPercentRange = errors.New("voyage: batch percent out of range [1, 100]")
	ErrBatchSpecHostsRange   = errors.New("voyage: batch hosts must be >= 1")
)

// batchSpecMaxDigits — ceiling on the numeric part's length. int64 has 19
// significant digits in the worst case; 9 safely covers any reasonable batch
// size (up to 999,999,999) and reliably fits in int on all target platforms,
// ruling out overflow before it even reaches strconv.Atoi.
const batchSpecMaxDigits = 9

// ParseBatchSpec parses the Voyage string batch field (S1 of the string batch fields).
//
// Grammar (fail-closed): trim the input string → strictly `^(\d+)(%?)$`. Suffix
// `%` ⇒ [BatchSpecPercent], value∈[1,100]; otherwise [BatchSpecHosts], value≥1.
// Any deviation (sign, dot, internal whitespace, extra characters, overflow) →
// [ErrBatchSpecMalformed]. Empty/whitespace string → [ErrBatchSpecEmpty]
// (the caller treats it as "not set", NOT as an input error).
//
// Pure function: no allocations beyond trim, no regexp (a manual scan is cheaper
// and gives an explicit overflow guard on the digit part's length).
func ParseBatchSpec(s string) (mode BatchSpecMode, value int, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, ErrBatchSpecEmpty
	}

	digits := s
	percent := false
	if last := len(s) - 1; s[last] == '%' {
		percent = true
		digits = s[:last]
	}
	// Empty digit part ("%"); empty after trim is already ruled out above.
	if digits == "" {
		return 0, 0, ErrBatchSpecMalformed
	}
	// ASCII digits only: rules out sign, dot, internal whitespace, a second `%`.
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, 0, ErrBatchSpecMalformed
		}
	}
	// Overflow guard on length BEFORE strconv: huge digit strings → malformed.
	if len(digits) > batchSpecMaxDigits {
		return 0, 0, ErrBatchSpecMalformed
	}

	n, convErr := strconv.Atoi(digits)
	if convErr != nil {
		return 0, 0, ErrBatchSpecMalformed
	}

	if percent {
		if n < 1 || n > 100 {
			return 0, 0, ErrBatchSpecPercentRange
		}
		return BatchSpecPercent, n, nil
	}
	if n < 1 {
		return 0, 0, ErrBatchSpecHostsRange
	}
	return BatchSpecHosts, n, nil
}
