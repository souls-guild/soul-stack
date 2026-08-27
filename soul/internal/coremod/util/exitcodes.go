package util

import (
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// ExitCodes is the set of process exit codes a verb-shell module accepts as
// success — the `exit_codes` param of core.exec.run and core.cmd.shell
// (NIM-687). A code outside the set fails the task.
//
// Spans rather than a plain []int64 because an author may write a range. The
// zero value accepts nothing; build one with [OptExitCodesParam], which never
// returns an empty set without also returning an error.
type ExitCodes []ExitCodeSpan

// ExitCodeSpan is one inclusive [Lo, Hi] run of accepted codes. An exact code
// is a span with Lo == Hi.
type ExitCodeSpan struct{ Lo, Hi int64 }

// DefaultExitCodes — what a task that says nothing gets: only 0 is success.
func DefaultExitCodes() ExitCodes { return ExitCodes{{Lo: 0, Hi: 0}} }

// Allows reports whether code belongs to the set.
func (e ExitCodes) Allows(code int) bool {
	for _, s := range e {
		if int64(code) >= s.Lo && int64(code) <= s.Hi {
			return true
		}
	}
	return false
}

// String renders the set the way an author writes it ("0", "0,2-5"). The
// failure message has to name the allowed set and not just the code that
// missed it, otherwise the operator cannot tell a wrong command from a wrong
// expectation.
func (e ExitCodes) String() string {
	if len(e) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(e))
	for _, s := range e {
		if s.Lo == s.Hi {
			parts = append(parts, strconv.FormatInt(s.Lo, 10))
			continue
		}
		parts = append(parts, strconv.FormatInt(s.Lo, 10)+"-"+strconv.FormatInt(s.Hi, 10))
	}
	return strings.Join(parts, ",")
}

// OptExitCodesParam reads the optional `exit_codes` list. Missing or null →
// [DefaultExitCodes].
//
// Elements are heterogeneous by design — which is why the manifest declares the
// param as a list with no item type:
//
//   - an integer is one exact code. Negative is accepted: Go reports -1 for a
//     process killed by a signal, so `exit_codes: [0, -1]` is expressible.
//   - a "lo-hi" string is an inclusive range, non-negative only. The string
//     form excludes negatives because "-1" cannot be told apart from a
//     malformed range; a negative code goes as a bare integer instead.
//
// An empty list is rejected rather than read as "accept everything" or
// "accept nothing": either reading turns a likely typo into a run that behaves
// nothing like the author meant.
//
// Where it is deliberately lenient, so the next reader does not mistake it for
// an oversight: a bare "7" parses as the exact code 7 (the string and integer
// forms overlap by design — YAML quoting is not the author's contract with us),
// strconv's own tolerances carry through ("+7", surrounding spaces), and a code
// above 255 is taken at face value even though no POSIX wait status can produce
// one. None of these can turn into silently-wrong behaviour: each still names
// exactly one code, and a code that cannot occur simply never matches. That is
// not the empty list's problem — an empty list changes the verdict on codes the
// author did not write down.
func OptExitCodesParam(params *structpb.Struct, key string) (ExitCodes, error) {
	if params == nil || params.Fields == nil {
		return DefaultExitCodes(), nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return DefaultExitCodes(), nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return DefaultExitCodes(), nil
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected a list of exit codes (integer or %q range), got %T",
			key, "lo-hi", v.Kind)
	}
	if len(lv.ListValue.Values) == 0 {
		return nil, fmt.Errorf("param %q: the list is empty, which would accept no exit code at all, "+
			"not even 0 — omit the param to accept only 0", key)
	}
	out := make(ExitCodes, 0, len(lv.ListValue.Values))
	for i, item := range lv.ListValue.Values {
		switch k := item.Kind.(type) {
		case *structpb.Value_NumberValue:
			f := k.NumberValue
			if f != float64(int64(f)) {
				return nil, fmt.Errorf("param %q[%d]: expected an integer exit code, got %v", key, i, f)
			}
			out = append(out, ExitCodeSpan{Lo: int64(f), Hi: int64(f)})
		case *structpb.Value_StringValue:
			span, err := parseExitCodeRange(k.StringValue)
			if err != nil {
				return nil, fmt.Errorf("param %q[%d]: %v", key, i, err)
			}
			out = append(out, span)
		default:
			return nil, fmt.Errorf("param %q[%d]: expected an integer or a %q range string, got %T",
				key, i, "lo-hi", item.Kind)
		}
	}
	return out, nil
}

// parseExitCodeRange accepts "N" and "lo-hi" (inclusive), non-negative.
func parseExitCodeRange(s string) (ExitCodeSpan, error) {
	t := strings.TrimSpace(s)
	malformed := fmt.Errorf("%q is not an exit code: want an integer (\"3\") or an inclusive range "+
		"(\"2-5\"), both non-negative; a negative code goes as a bare integer, not a string", t)

	loText, hiText, isRange := strings.Cut(t, "-")
	lo, err := parseExitCode(loText)
	if err != nil {
		return ExitCodeSpan{}, malformed
	}
	if !isRange {
		return ExitCodeSpan{Lo: lo, Hi: lo}, nil
	}
	hi, err := parseExitCode(hiText)
	if err != nil {
		return ExitCodeSpan{}, malformed
	}
	if hi < lo {
		return ExitCodeSpan{}, fmt.Errorf("range %q runs backwards (%d > %d), so it accepts nothing", t, lo, hi)
	}
	return ExitCodeSpan{Lo: lo, Hi: hi}, nil
}

func parseExitCode(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a non-negative integer")
	}
	return n, nil
}
