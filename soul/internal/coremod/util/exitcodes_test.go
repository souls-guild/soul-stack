package util_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestOptExitCodesParam_DefaultsToZeroOnly(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"absent": {"cmd": "true"},
		"null":   {"cmd": "true", "exit_codes": nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := util.OptExitCodesParam(mustStruct(t, params), "exit_codes")
			if err != nil {
				t.Fatalf("OptExitCodesParam: %v", err)
			}
			if !got.Allows(0) {
				t.Error("0 is not accepted by the default set")
			}
			for _, code := range []int{1, 2, 42, -1} {
				if got.Allows(code) {
					t.Errorf("%d is accepted by the default set; the default is [0] alone", code)
				}
			}
		})
	}
	if _, err := util.OptExitCodesParam(nil, "exit_codes"); err != nil {
		t.Fatalf("OptExitCodesParam(nil): %v", err)
	}
}

func TestOptExitCodesParam_Forms(t *testing.T) {
	cases := []struct {
		name    string
		value   []any
		allowed []int
		denied  []int
	}{
		{"exact", []any{0}, []int{0}, []int{1, -1}},
		{"list", []any{0, 1, 2}, []int{0, 1, 2}, []int{3, -1}},
		{"range", []any{"2-5"}, []int{2, 3, 4, 5}, []int{0, 1, 6}},
		{"mixed", []any{0, "2-5"}, []int{0, 2, 5}, []int{1, 6}},
		{"single-code string", []any{"7"}, []int{7}, []int{0, 6, 8}},
		// Go reports -1 for a process killed by a signal, so the bare-integer
		// form has to reach negatives — the string form deliberately cannot.
		{"negative", []any{0, -1}, []int{0, -1}, []int{1}},
		// One-element range: an author who writes it means that code, not a typo.
		{"degenerate range", []any{"4-4"}, []int{4}, []int{3, 5}},
		{"whitespace", []any{" 2 - 5 "}, []int{2, 5}, []int{1, 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := util.OptExitCodesParam(
				mustStruct(t, map[string]any{"exit_codes": tc.value}), "exit_codes")
			if err != nil {
				t.Fatalf("OptExitCodesParam(%v): %v", tc.value, err)
			}
			for _, code := range tc.allowed {
				if !got.Allows(code) {
					t.Errorf("%v: %d rejected, want accepted (set renders as %s)", tc.value, code, got)
				}
			}
			for _, code := range tc.denied {
				if got.Allows(code) {
					t.Errorf("%v: %d accepted, want rejected (set renders as %s)", tc.value, code, got)
				}
			}
		})
	}
}

func TestOptExitCodesParam_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		value any
		// want is a fragment of the message. Each malformed value has its own
		// cause, and an author reading "invalid exit_codes" learns nothing about
		// which of them they hit.
		want string
	}{
		{"empty list", []any{}, "not even 0"},
		{"not a list", 3, "expected a list"},
		{"string, not a list", "0-3", "expected a list"},
		{"fractional", []any{1.5}, "expected an integer"},
		{"boolean element", []any{true}, "expected an integer or"},
		{"nested list", []any{[]any{0}}, "expected an integer or"},
		{"not a number", []any{"abc"}, "not an exit code"},
		{"open range", []any{"2-"}, "not an exit code"},
		{"negative as string", []any{"-1"}, "not an exit code"},
		{"backwards range", []any{"5-2"}, "runs backwards"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := util.OptExitCodesParam(
				mustStruct(t, map[string]any{"exit_codes": tc.value}), "exit_codes")
			if err == nil {
				t.Fatalf("OptExitCodesParam(%v) = %v, want an error", tc.value, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q, so it does not say what is wrong with the value",
					err, tc.want)
			}
		})
	}
}

// The failure message names the ALLOWED SET, not just the code that missed it:
// without it the operator cannot tell a wrong command from a wrong expectation.
func TestExitCodes_String(t *testing.T) {
	for _, tc := range []struct {
		value []any
		want  string
	}{
		{[]any{0}, "0"},
		{[]any{0, 1}, "0,1"},
		{[]any{"2-5"}, "2-5"},
		{[]any{0, "2-5"}, "0,2-5"},
		{[]any{0, -1}, "0,-1"},
	} {
		got, err := util.OptExitCodesParam(
			mustStruct(t, map[string]any{"exit_codes": tc.value}), "exit_codes")
		if err != nil {
			t.Fatalf("OptExitCodesParam(%v): %v", tc.value, err)
		}
		if got.String() != tc.want {
			t.Errorf("%v renders as %q, want %q", tc.value, got, tc.want)
		}
	}
	if got := util.DefaultExitCodes().String(); got != "0" {
		t.Errorf("DefaultExitCodes renders as %q, want %q", got, "0")
	}
	// The zero value accepts nothing, and says so — it must never render as an
	// empty string, which in a message would read as "no restriction".
	if got := (util.ExitCodes{}).String(); got != "(none)" {
		t.Errorf("the empty set renders as %q, want %q", got, "(none)")
	}
}
