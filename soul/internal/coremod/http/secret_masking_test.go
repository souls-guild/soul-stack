package http_test

import (
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// The masker replaces whole values, so the question that decides whether it
// holds is "can two occurrences overlap?" — and the answer is yes, from three
// independent directions:
//
//   - two DIFFERENT values can abut and share a byte, which the endpoint can
//     arrange because it knows the Content-Type it was sent;
//   - ONE value overlaps ITSELF whenever it has a border (its own prefix is
//     also its suffix), so a body echoing it twice back to back is enough —
//     no second header needed, which puts probe in range too;
//   - the CUT manufactures the shape on its own: a value with a period, cut
//     mid-repeat, leaves overlapping occurrences where there were none.
//
// A matcher that consumes matches left to right — strings.Replacer, which this
// used to be — cannot mask the second occurrence of an overlapping pair: it has
// already moved its cursor past that offset. What sticks out stays in
// output.body verbatim, up to len(value)-1 plaintext bytes of a credential.
//
// So every guard below asserts on the WHOLE output, not on a tail: an overlap
// leak sits mid-text, not after the last mask, and a tail-only detector reports
// clean on it. assertNoValueRun sweeps every substring of the value rather than
// its prefixes, because the leftover of an overlap is a suffix.

// assertNoValueRun fails if any run of at least minLen consecutive bytes taken
// from anywhere inside value occurs anywhere in body. Longest runs are reported
// first so the failure names the worst leak, not the smallest coincidence.
func assertNoValueRun(t *testing.T, body, value string, minLen int) {
	t.Helper()
	for size := len(value); size >= minLen; size-- {
		for start := 0; start+size <= len(value); start++ {
			if i := strings.Index(body, value[start:start+size]); i >= 0 {
				t.Fatalf("a %d-byte run of the sensitive value survived at offset %d (value[%d:%d]); body around it: %q",
					size, i, start, start+size, clipAround(body, i))
			}
		}
	}
}

func clipAround(body string, i int) string {
	lo, hi := max(0, i-24), min(len(body), i+48)
	return body[lo:hi]
}

func applyOnce(t *testing.T, state string, params map[string]any, body string) *pluginv1.ApplyEvent {
	t.Helper()
	d := &fakeDoer{status: 200, body: []byte(body)}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: state, Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	return ev
}

// Two different values sharing a boundary byte. The endpoint is handed the
// Content-Type it must echo, so lining its last byte up with the token's first
// one costs it nothing.
func TestApply_Request_OverlappingValuesAreBothMasked(t *testing.T) {
	const contentType = "application/json; charset=utf-8" // ends with '8'
	const token = "8f3a1c7e5b2d40869f3a1c7e5b2d4086"      // starts with '8'
	if contentType[len(contentType)-1] != token[0] {
		t.Fatal("test setup: the two values no longer overlap, this asserts nothing")
	}
	// The two occurrences share exactly one byte, so their coverage is one run.
	raw := `{"echo":"` + contentType + token[1:] + `"}`

	ev := applyOnce(t, "request", map[string]any{
		"url": "https://example.com/v1/resource", "method": "POST",
		"content_type": contentType,
		"headers":      map[string]any{"X-Api-Key": token},
	}, raw)

	body := ev.Output.Fields["body"].GetStringValue()
	if ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=true: this path must not involve any cut")
	}
	if want := `{"echo":"` + maskedValue + `"}`; body != want {
		t.Fatalf("overlapping values not masked as one run:\n got %q\nwant %q", body, want)
	}
	assertNoValueRun(t, body, token, 4)
	assertNoValueRun(t, body, contentType, 4)
}

// One value, no content_type, no truncation — and it still overlaps itself,
// because its first byte equals its last. This is the shape that puts probe in
// range: a single header is all it takes.
func TestApply_Probe_SelfOverlappingValueIsFullyMasked(t *testing.T) {
	const token = "8f3a1c7e5b2d40869f3a1c7e5b2d4088" // first byte == last byte
	if token[0] != token[len(token)-1] {
		t.Fatal("test setup: the value no longer overlaps itself, this asserts nothing")
	}
	// Occurrences at 0 and at len(token)-1, sharing one byte.
	raw := token + token[1:]

	ev := applyOnce(t, "probe", map[string]any{
		"url":     "https://example.com/healthz",
		"headers": map[string]any{"X-Api-Key": token},
	}, raw)

	body := ev.Output.Fields["body"].GetStringValue()
	if body != maskedValue {
		t.Fatalf("self-overlapping value not masked as one run:\n got %q\nwant %q", body, maskedValue)
	}
	assertNoValueRun(t, body, token, 4)
}

// The cut is not just something the masker has to survive — it MANUFACTURES the
// overlap. A value with a period, cut mid-repeat, leaves occurrences that share
// bytes where the intact body had two clean ones. That is also why cutFragments
// may treat a complete value at the cut as "not a fragment": it is only safe
// while the masker unions overlapping occurrences.
func TestApply_Request_CutCreatesOverlappingOccurrences(t *testing.T) {
	const token = "secretsecret" // period 6: overlaps itself every 6 bytes
	const head = 64*1024 - 18
	// 18 bytes of the repeat survive the cap: occurrences at +0 and +6.
	raw := strings.Repeat("Q", head) + strings.Repeat(token, 2) + strings.Repeat("W", 94)
	if len(raw) <= 64*1024 {
		t.Fatalf("test setup: raw=%d, want >64KiB so the cut actually happens", len(raw))
	}
	if strings.ContainsAny("QW", token) {
		t.Fatal("test setup: padding shares bytes with the secret, the assertion would fire on it")
	}

	ev := applyOnce(t, "request", map[string]any{
		"url": "https://example.com/v1/resource", "method": "PUT",
		"headers": map[string]any{"X-Consul-Token": token},
	}, raw)

	body := ev.Output.Fields["body"].GetStringValue()
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false: the test did not exercise the cut")
	}
	if want := strings.Repeat("Q", head) + maskedValue; body != want {
		t.Fatalf("cut-made overlap leaked; body ends %q, want it to end %q",
			body[max(0, len(body)-40):], want[max(0, len(want)-40):])
	}
	assertNoValueRun(t, body, token, 2)
}

// The same shape with period 1. Here the endpoint does not need to know the
// value at all — it only has to answer with a run of one repeated byte ending
// at the cap, which padding, a hex dump of zeroes or any binary blob does by
// accident.
func TestApply_Request_CutInsideSingleByteRunDoesNotLeak(t *testing.T) {
	token := strings.Repeat("a", 16)
	const head = 64*1024 - 20
	raw := strings.Repeat("Q", head) + strings.Repeat("a", 32) + strings.Repeat("W", 100)
	if len(raw) <= 64*1024 {
		t.Fatalf("test setup: raw=%d, want >64KiB so the cut actually happens", len(raw))
	}

	ev := applyOnce(t, "request", map[string]any{
		"url": "https://example.com/v1/resource", "method": "PATCH",
		"headers": map[string]any{"X-Token": token},
	}, raw)

	body := ev.Output.Fields["body"].GetStringValue()
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false: the test did not exercise the cut")
	}
	if strings.Contains(body, "a") {
		i := strings.Index(body, "a")
		t.Fatalf("bytes of the single-byte-run value survived at offset %d: %q", i, clipAround(body, i))
	}
}

// A header value can carry a vault-ref of its own — an unresolved
// `${ vault(...) }` reaches the wire as written. Masking refs first would
// rewrite the MIDDLE of that value and leave the whole-value matcher nothing to
// match, stranding the value's leading bytes in plaintext.
func TestApply_Request_ValueContainingVaultRefIsMaskedWhole(t *testing.T) {
	const token = "Bearer vault:secret/consul#token"
	raw := `{"echo":"` + token + `"}`

	ev := applyOnce(t, "request", map[string]any{
		"url": "https://example.com/v1/resource", "method": "POST",
		"headers": map[string]any{"Authorization": token},
	}, raw)

	body := ev.Output.Fields["body"].GetStringValue()
	if want := `{"echo":"` + maskedValue + `"}`; body != want {
		t.Fatalf("value carrying a vault-ref was not masked whole:\n got %q\nwant %q", body, want)
	}
	if strings.Contains(body, "Bearer") {
		t.Fatal("the value's leading bytes survived: refs were masked before header values")
	}
}

// The descent that repairs a cut must not rescan the body per step. The tail
// below is one long run of continuation bytes, and the roll-back to the last
// complete rune removes exactly ONE of them per step — so a per-step rescan
// turns into a full pass over 64 KiB per byte. That measured ~15s of CPU here;
// the budget is set far above the repaired cost (milliseconds) and far below
// the quadratic one, so it separates the two without racing a loaded machine.
func TestApply_Request_CutRepairDoesNotRescanPerStep(t *testing.T) {
	raw := strings.Repeat("\xbf", 64*1024+1) // lone continuation bytes
	const budget = 3 * time.Second

	start := time.Now()
	ev := applyOnce(t, "request", map[string]any{
		"url": "https://example.com/v1/resource", "method": "POST",
		"headers": map[string]any{
			"X-Consul-Token": "8f3a1c7e5b2d40869f3a1c7e5b2d4086",
			"Authorization":  "Bearer 0123456789abcdef0123456789abcdef",
		},
	}, raw)
	elapsed := time.Since(start)

	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false: the test did not reach the cut repair at all")
	}
	if elapsed > budget {
		t.Fatalf("cut repair took %v (budget %v): the descent is rescanning the body per step, "+
			"which the response body — endpoint-controlled — turns into a remote CPU sink", elapsed, budget)
	}
}
