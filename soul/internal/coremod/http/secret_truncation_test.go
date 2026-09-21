package http_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// The response body is cut at a hard byte offset BEFORE redaction runs, so a
// sensitive value straddling the cap survives as a plaintext prefix, and
// redactHeaderValues — a whole-value matcher — is structurally blind to it.
//
// Two things about these guards are deliberate.
//
// First, the secret carries a multi-byte rune. A cut landing inside one is not
// an exotic sub-case: it is the case that defeats a repair applied AFTER
// sanitation, because trimPartialRune drops a single byte and sanitizeBody
// rewrites whatever is left as U+FFFD, leaving a fragment that no longer looks
// like a prefix of anything. keep is swept over every offset so both the plain
// and the mid-rune cuts are covered.
//
// Second, the detector uses strings.Contains over the region past the last
// mask, never strings.HasSuffix. HasSuffix is the operation the code under test
// performs; a guard built on it inherits that code's blind spots by
// construction and reports clean on a live leak. The padding is made only of
// bytes that never occur in the secret, so any hit in that region is real —
// which also means no sensitive VALUE here may be built from a padding byte. A
// one-byte value equal to the padding masks every byte of it, pushing the last
// mask to the end of the body and leaving the detector with nothing to read.

// straddleSecret spans a multi-byte rune (U+20AC, 3 bytes) at offsets 13..15.
// Neither 'x' nor 'y' — the padding bytes — occurs anywhere in it.
const straddleSecret = "consul-token-€-9f3a1c7e5b2d4086"

const maskedValue = "***MASKED***"

// straddleBody returns a body in which secret occurs twice: once wholly inside
// the cap (so redaction is provably still running) and once positioned so that
// exactly its first `keep` bytes land before the cap.
func straddleBody(secret string, keep int) string {
	pad := strings.Repeat("x", 64*1024-keep-len(secret))
	return secret + pad + secret + strings.Repeat("y", 4096)
}

func assertNoSecretFragmentTail(t *testing.T, body, secret string) {
	t.Helper()
	tail := body
	if i := strings.LastIndex(body, maskedValue); i >= 0 {
		tail = body[i+len(maskedValue):]
	}
	for size := len(secret); size > 0; size-- {
		if strings.Contains(tail, secret[:size]) {
			t.Fatalf("a %d-byte prefix of the secret survived the cut; body ends %q",
				size, tail[max(0, len(tail)-2*len(secret)):])
		}
	}
}

func assertRedactionRan(t *testing.T, ev *pluginv1.ApplyEvent) string {
	t.Helper()
	if ev.Failed {
		t.Fatalf("failed=true: %s", ev.Message)
	}
	if !ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=false: the test did not exercise the truncation path")
	}
	body := ev.Output.Fields["body"].GetStringValue()
	if !strings.Contains(body, maskedValue) {
		t.Fatal("no mask in the body: the whole-value redaction did not run at all")
	}
	return body
}

// applyStraddle runs one verb against a body whose second copy of secret is cut
// after `keep` bytes, and returns the operator-visible body.
func applyStraddle(t *testing.T, state, secret string, keep int, params map[string]any) string {
	t.Helper()
	d := &fakeDoer{status: 200, body: []byte(straddleBody(secret, keep))}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: state, Params: mustStruct(t, params),
	}, stream); err != nil {
		t.Fatalf("keep=%d Apply: %v", keep, err)
	}
	return assertRedactionRan(t, stream.Last())
}

// Rune width at the cut is the axis that defeated the first repair: a cut lands
// inside a multi-byte rune, trimPartialRune removes a single byte, and whatever
// is left becomes U+FFFD — so only widths ≥3 can strand a fragment, and a guard
// that tests ASCII alone reports clean. Every width is swept over every cut
// offset; an off-by-one at either bound leaves exactly one fragment size behind
// and a sampled sweep cannot see it.
func TestApply_Request_TruncatedBodyDropsStraddlingHeaderValue(t *testing.T) {
	for _, secret := range []string{
		"consul-token-9f3a1c7e5b2d4086", // pure ASCII
		"consul-token-é-9f3a1c7e5b2d40", // U+00E9, 2 bytes
		"consul-token-€-9f3a1c7e5b2d40", // U+20AC, 3 bytes
		"consul-token-😀-9f3a1c7e5b2d4",  // U+1F600, 4 bytes
		"consul-token-€😀é-9f3a1c7e5b2",  // three runes in a row
		"secretsecret",                  // self-overlapping, period 6
		"abcabcabcabc",                  // self-overlapping, period 3
	} {
		for keep := 1; keep < len(secret); keep++ {
			body := applyStraddle(t, "request", secret, keep, map[string]any{
				"url": "https://example.com/v1/agent/service/register", "method": "PUT",
				"headers": map[string]any{"X-Consul-Token": secret},
			})
			assertNoSecretFragmentTail(t, body, secret)
		}
	}
}

// Two sensitive values where one is a proper prefix of the other. The longer
// must be consumed first: matching the shorter one leaves the remainder of the
// longer sitting in the output.
func TestApply_Request_TruncatedBodyDropsOverlappingHeaderValues(t *testing.T) {
	const short = "consul-token-9f3a"
	const long = short + "-€-1c7e5b2d4086"
	for keep := 1; keep < len(long); keep++ {
		body := applyStraddle(t, "request", long, keep, map[string]any{
			"url": "https://example.com/v1/agent/service/register", "method": "PUT",
			"headers": map[string]any{"A-Short": short, "B-Long": long},
		})
		assertNoSecretFragmentTail(t, body, long)
	}
}

// The shortest value that can strand anything is two bytes: it has exactly one
// proper prefix. Guarding that boundary needs its own body, not a sweep entry —
// a two-byte value cannot be masked without the body GROWING by ten bytes past
// the cap, and capBody then trims the one leaked byte off the end, so the leak
// hides behind an unrelated mechanism and the case gates nothing. Here a long
// co-value carries the mask (30 bytes in, 12 out — the body shrinks), and the
// short value straddles the cap on its own.
func TestApply_Request_TruncatedBodyDropsShortestStraddlingValue(t *testing.T) {
	const short = "ZQ"
	if strings.ContainsAny(straddleSecret+"xy", short) {
		t.Fatalf("test setup: %q must share no byte with the padding or the co-value", short)
	}
	// Exactly one byte of short lands before the cap.
	pad := strings.Repeat("x", 64*1024-len(straddleSecret)-1)
	d := &fakeDoer{status: 200, body: []byte(straddleSecret + pad + short + strings.Repeat("y", 4096))}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request", Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/agent/service/register", "method": "PUT",
			"headers": map[string]any{"A-Long": straddleSecret, "B-Short": short},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body := assertRedactionRan(t, stream.Last())
	if n := len(body); n > 64*1024 {
		t.Fatalf("body=%d bytes: the masked body must stay under the cap, or capBody hides the leak", n)
	}
	assertNoSecretFragmentTail(t, body, short)
}

// The byte cap is NOT the only cut in this pipeline, and every cut can strand a
// plaintext prefix. trimPartialRune cuts again, immediately after it: a fragment
// sitting just before an UNRELATED multi-byte rune is invisible to a repair that
// only inspects the first cut — at that moment the tail is `frag`+`0xE2` and
// matches nothing — and the trim then re-exposes `frag` at the new boundary.
//
// The secret here is pure ASCII deliberately: that makes every high byte after
// the fragment structurally unrelated to it, so this is the coincidental case
// and not a straddle wearing its clothes.
//
// `kept` is swept because trimPartialRune drops ONE byte per call. A repair
// invoked once after it closes only the one-byte remainder; two and three bytes
// of a cut rune still reach sanitizeBody, which rewrites them into U+FFFD and
// leaves the fragment sitting in front of it, in plaintext.
func TestApply_Request_TruncatedBodyDropsFragmentReexposedByRuneTrim(t *testing.T) {
	const secret = "SECRET-TOKEN-abcdef"
	for _, r := range []string{"€", "😀"} {
		for kept := 1; kept < len(r); kept++ {
			for _, n := range []int{1, 4, 13, len(secret) - 1} {
				pad := strings.Repeat("q", 64*1024-len(secret)-n-kept)
				raw := secret + pad + secret[:n] + r + strings.Repeat("y", 4096)
				if raw[64*1024-1] < 0x80 {
					t.Fatalf("kept=%d n=%d: setup inert — the byte at the cut is %#x, want a rune byte",
						kept, n, raw[64*1024-1])
				}
				d := &fakeDoer{status: 200, body: []byte(raw)}
				stream := &internaltest.ApplyStream{}
				if err := newModule(d).Apply(&pluginv1.ApplyRequest{
					State: "request", Params: mustStruct(t, map[string]any{
						"url": "https://example.com/v1/resource", "method": "POST",
						"headers": map[string]any{"X-Token": secret},
					}),
				}, stream); err != nil {
					t.Fatalf("kept=%d n=%d Apply: %v", kept, n, err)
				}
				assertNoSecretFragmentTail(t, assertRedactionRan(t, stream.Last()), secret)
			}
		}
	}
}

// The THIRD cut: capBody. Masking can GROW the text (any value shorter than the
// mask), and the second cap then lands wherever it lands — including right after
// a coincidental prefix of a value. The claim that this cut can only bisect a
// mask holds for whole values only; a prefix of one is not a mask.
//
// This body is exactly at the cap, so the raw path never runs: whatever the
// guard sees comes from capBody alone, and no earlier repair can be credited.
func TestApply_Request_OutputCapCutDropsStrandedFragment(t *testing.T) {
	const secret = "SECRET-TOKEN-abcdef"
	const short = "tok" // 3 bytes in, 12 out: this is what pushes the text over
	const frag = 10     // bytes of secret left sitting at the second cut
	// Raw length is exactly the cap (so rawTruncated stays false), and the
	// growth from masking `short` places the second cut at the end of frag.
	trailer := strings.Repeat("y", 9)
	pad := strings.Repeat("q", 64*1024-len(short)-frag-len(trailer))
	raw := short + pad + secret[:frag] + trailer
	if len(raw) != 64*1024 {
		t.Fatalf("test setup: raw=%d, want exactly the cap — otherwise the raw path runs too", len(raw))
	}
	d := &fakeDoer{status: 200, body: []byte(raw)}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request", Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
			"headers": map[string]any{"X-Token": secret, "X-Short": short},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertNoSecretFragmentTail(t, assertRedactionRan(t, stream.Last()), secret)
}

// Degenerate sensitive values must not crash the repair or eat the body: an
// empty value carries nothing, a single byte has no proper prefix at all, and a
// value longer than everything before the cut must not drive the length
// negative. None of them may occur in the padding — see the note above.
func TestApply_Request_TruncatedBodyDegenerateSensitiveValues(t *testing.T) {
	d := &fakeDoer{status: 200, body: []byte(straddleBody(straddleSecret, 7))}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/agent/service/register", "method": "PUT",
			"headers": map[string]any{
				"X-Empty":  "",
				"X-One":    "Q",
				"X-Blank":  "   ",
				"X-Huge":   strings.Repeat("z", 128*1024),
				"X-Consul": straddleSecret,
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body := assertRedactionRan(t, stream.Last())
	if n := strings.Count(body, maskedValue); n != 1 {
		t.Fatalf("masks=%d, want exactly 1: a degenerate value must not mask the body away", n)
	}
	assertNoSecretFragmentTail(t, body, straddleSecret)
}

// A value ending EXACTLY at the cut is not a fragment — whole-value masking
// covers it, and the repair must leave it alone. A value with a period also
// ends with its own proper prefix, so a strip running BEFORE the mask bites
// into the complete value, eats the second half and leaves the first half in
// plaintext: a value the masker alone would have handled is destroyed by the
// very code meant to protect it. The mask count is the known-bad — a damaged
// value no longer matches, so nothing is masked at all.
func TestApply_Request_TruncatedBodyMasksValueEndingExactlyAtCap(t *testing.T) {
	// Every value here is at least as long as the mask. A shorter one ending at
	// the cut makes the masked body outgrow the cap, capBody bisects the mask,
	// and the count below would be measuring that instead of the repair.
	for _, secret := range []string{
		"secretsecret",            // period 6
		"zzzzzzzzzzzzzzzz",        // period 1
		"abcabcabcabc",            // period 3
		"consul-token-€-9f3a1c7e", // aperiodic control
	} {
		pad := strings.Repeat("Q", 64*1024-len(secret))
		d := &fakeDoer{status: 200, body: []byte(pad + secret + strings.Repeat("W", 4096))}
		stream := &internaltest.ApplyStream{}
		if err := newModule(d).Apply(&pluginv1.ApplyRequest{
			State: "request",
			Params: mustStruct(t, map[string]any{
				"url": "https://example.com/v1/resource", "method": "POST",
				"headers": map[string]any{"X-Tok": secret},
			}),
		}, stream); err != nil {
			t.Fatalf("secret=%q Apply: %v", secret, err)
		}
		body := assertRedactionRan(t, stream.Last())
		if n := strings.Count(body, maskedValue); n != 1 {
			t.Fatalf("secret=%q: masks=%d, want exactly 1 — the complete value must be masked, not eaten",
				secret, n)
		}
		assertNoSecretFragmentTail(t, body, secret)
	}
}

// One cut exposing the next. `chained` ends exactly at the cap, so it is not a
// fragment and must be left to the masker — but `trailer` begins with the two
// bytes that end it, so those two ARE a fragment. Removing them bites into the
// complete value and strands its 23-byte head in plaintext unless the repair
// re-examines the new cut. A single-pass repair leaves exactly that behind.
func TestApply_Request_TruncatedBodyRepeatsUntilTheCutIsClean(t *testing.T) {
	const chained = "consul-token-€-9f3a1c7e"
	const trailer = "7e-second-token-value"
	if !strings.HasPrefix(trailer, chained[len(chained)-2:]) {
		t.Fatalf("test setup: %q must begin with the last bytes of %q", trailer, chained)
	}
	pad := strings.Repeat("x", 64*1024-2*len(chained))
	d := &fakeDoer{status: 200, body: []byte(chained + pad + chained + strings.Repeat("y", 4096))}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
			"headers": map[string]any{"A-Chained": chained, "B-Trailer": trailer},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertNoSecretFragmentTail(t, assertRedactionRan(t, stream.Last()), chained)
}

// The descent in trimCutTail must run to a fixed point at the OUTPUT cut too,
// not only at the raw one. Both cuts call the same descent, so a one-step
// version would be caught at either — but the two cuts see different text (the
// output cut sees it after masking has expanded it), and this is the one that
// pins the second call site down. Two fragments are stacked so that removing
// the outer uncovers the inner.
func TestApply_Request_OutputCapCutRepeatsUntilTheCutIsClean(t *testing.T) {
	const alpha = "SECRET-ALPHA-0123456789"
	const beta = "ZQ-BETA-9876543210"
	const short = "tok" // 3 bytes in, 12 out: this is what pushes the text over
	stack := alpha[:12] + beta[:2]
	trailer := strings.Repeat("y", 9)
	pad := strings.Repeat("q", 64*1024-len(maskedValue)-len(stack))
	raw := short + pad + stack + trailer
	if len(raw) != 64*1024 {
		t.Fatalf("test setup: raw=%d, want exactly the cap — otherwise the raw path runs too", len(raw))
	}
	if strings.ContainsAny(pad+trailer, alpha[:1]+beta[:1]) {
		t.Fatalf("test setup: padding carries a leading secret byte — the assertion would fire on it")
	}
	d := &fakeDoer{status: 200, body: []byte(raw)}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request", Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
			"headers": map[string]any{"X-Alpha": alpha, "X-Beta": beta, "X-Short": short},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	body := assertRedactionRan(t, stream.Last())
	assertNoSecretFragmentTail(t, body, alpha)
	assertNoSecretFragmentTail(t, body, beta)
}

// probe shares do() with request, but the wiring is per-verb: keep=14 and 15 cut
// inside U+20AC, keep=13 stops just before it.
func TestApply_Probe_TruncatedBodyDropsStraddlingHeaderValue(t *testing.T) {
	for _, keep := range []int{13, 14, 15} {
		body := applyStraddle(t, "probe", straddleSecret, keep, map[string]any{
			"url":     "https://example.com/v1/agent/checks",
			"headers": map[string]any{"X-Consul-Token": straddleSecret},
		})
		assertNoSecretFragmentTail(t, body, straddleSecret)
	}
}

// content_type is the second source of an effective header value (it overrides
// headers["Content-Type"] on the wire), so it needs its own known-bad: a fix
// that only walks the headers map leaves this path leaking.
func TestApply_Request_TruncatedBodyDropsStraddlingContentType(t *testing.T) {
	const ct = "application/vnd.acme.private+json"
	for _, keep := range []int{1, 11, len(ct) - 1} {
		body := applyStraddle(t, "request", ct, keep, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
			"content_type": ct, "body": "{}",
		})
		assertNoSecretFragmentTail(t, body, ct)
	}
}

// A body that was NOT truncated must keep a coincidental trailing prefix of a
// header value: the tail-strip is a truncation repair, not a second masker.
// The literal below really does end with straddleSecret[:20] — otherwise this
// guard would hold nothing, since an ungated strip only bites on a real prefix.
func TestApply_Request_UntruncatedBodyKeepsCoincidentalPrefix(t *testing.T) {
	const coincidental = "done consul-token-€-9f3"
	if !strings.HasSuffix(coincidental, straddleSecret[:20]) {
		t.Fatalf("test setup: %q must end with a proper prefix of the secret", coincidental)
	}
	d := &fakeDoer{status: 200, body: []byte(coincidental)}
	stream := &internaltest.ApplyStream{}
	if err := newModule(d).Apply(&pluginv1.ApplyRequest{
		State: "request",
		Params: mustStruct(t, map[string]any{
			"url": "https://example.com/v1/resource", "method": "POST",
			"headers": map[string]any{"X-Consul-Token": straddleSecret},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Output.Fields["truncated"].GetBoolValue() {
		t.Fatal("truncated=true for a small body")
	}
	if got := ev.Output.Fields["body"].GetStringValue(); got != coincidental {
		t.Fatalf("body=%q, want it untouched", got)
	}
}
