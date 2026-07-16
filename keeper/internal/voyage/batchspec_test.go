package voyage

import (
	"errors"
	"testing"
)

// ParseBatchSpec — pure parser for the string batch field (S1 of the string batch fields).
// Grammar fail-closed: trim → `^(\d+)(%?)$`; `%` → percent∈[1,100], otherwise
// hosts≥1. Tests written BEFORE the implementation (TDD).

func TestParseBatchSpec_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantMode  BatchSpecMode
		wantValue int
	}{
		{"5", BatchSpecHosts, 5},
		{"20%", BatchSpecPercent, 20},
		{"100%", BatchSpecPercent, 100},
		{"1%", BatchSpecPercent, 1},
		{" 7 ", BatchSpecHosts, 7},
		{"1", BatchSpecHosts, 1},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			mode, value, err := ParseBatchSpec(tc.in)
			if err != nil {
				t.Fatalf("ParseBatchSpec(%q) error = %v, want nil", tc.in, err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %v, want %v", mode, tc.wantMode)
			}
			if value != tc.wantValue {
				t.Errorf("value = %d, want %d", value, tc.wantValue)
			}
		})
	}
}

func TestParseBatchSpec_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr error
	}{
		{"", ErrBatchSpecEmpty},
		{"   ", ErrBatchSpecEmpty},
		{"0", ErrBatchSpecHostsRange},
		{"0%", ErrBatchSpecPercentRange},
		{"150%", ErrBatchSpecPercentRange},
		{"101%", ErrBatchSpecPercentRange},
		{"5.5", ErrBatchSpecMalformed},
		{"5.5%", ErrBatchSpecMalformed},
		{"-3", ErrBatchSpecMalformed},
		{"abc", ErrBatchSpecMalformed},
		{"%", ErrBatchSpecMalformed},
		{"5 %", ErrBatchSpecMalformed},
		{"+5", ErrBatchSpecMalformed},
		{"5%%", ErrBatchSpecMalformed},
		{"%5", ErrBatchSpecMalformed},
		// Overflow: a huge number → malformed (not an strconv panic, not a silent clamp).
		{"99999999999999999999", ErrBatchSpecMalformed},
		{"99999999999999999999%", ErrBatchSpecMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, _, err := ParseBatchSpec(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ParseBatchSpec(%q) error = %v, want %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
