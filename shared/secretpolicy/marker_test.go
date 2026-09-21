package secretpolicy

import (
	"strings"
	"testing"
)

// TestMarker_RoundTrip — the travelling form reconstructs the policy exactly. The
// alphabet is written out rather than named, so no charset NAME has to mean the same
// thing on both sides of the boundary.
func TestMarker_RoundTrip(t *testing.T) {
	for _, charset := range []string{CharsetAlphanumeric, CharsetHex, CharsetBase64URL, CharsetASCIIPrintableSafe} {
		t.Run(charset, func(t *testing.T) {
			want, err := Parse(map[string]any{"length": 41, "charset": charset}, Default())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, isMarker, err := FromMarker(Marker(want))
			if err != nil {
				t.Fatalf("FromMarker: %v", err)
			}
			if !isMarker {
				t.Fatal("Marker() output not recognised as a marker")
			}
			if got.Length != want.Length || string(got.Alphabet) != string(want.Alphabet) {
				t.Fatalf("round trip changed the policy: %d/%q → %d/%q",
					want.Length, string(want.Alphabet), got.Length, string(got.Alphabet))
			}
			// A round trip alone is symmetric: Marker and FromMarker agreeing on the
			// WRONG wire shape passes it. The payload crosses params→protobuf→a module
			// in another process, so assert the field names and values on the wire.
			payload, ok := Marker(want)[MarkerKey].(map[string]any)
			if !ok {
				t.Fatalf("marker payload is %T, want map[string]any", Marker(want)[MarkerKey])
			}
			if payload["allowed_chars"] != string(want.Alphabet) {
				t.Errorf("wire allowed_chars = %v, want %q", payload["allowed_chars"], string(want.Alphabet))
			}
			if payload["length"] != want.Length {
				t.Errorf("wire length = %v, want %d", payload["length"], want.Length)
			}
			// The alphabet travels spelled out — no charset NAME both sides must agree on.
			if _, named := payload["charset"]; named {
				t.Error("wire carries a charset name; the alphabet must be written out")
			}
		})
	}
}

// TestFromMarker_SurvivesStructpbNumbers — the marker crosses params→protobuf, where an
// integer comes back as a float. Length must survive that trip; the alternative is a
// request that parses on one side of the wire and not the other.
func TestFromMarker_SurvivesStructpbNumbers(t *testing.T) {
	wire := map[string]any{MarkerKey: map[string]any{
		"length":        float64(24),
		"allowed_chars": "abcdef",
	}}
	got, isMarker, err := FromMarker(wire)
	if err != nil || !isMarker {
		t.Fatalf("FromMarker = (%v, %v, %v), want a parsed marker", got, isMarker, err)
	}
	if got.Length != 24 {
		t.Fatalf("length = %d, want 24", got.Length)
	}
}

// TestFromMarker_NotAMarker — ordinary data is left alone. `isMarker=false` means "keep
// this value", so a false positive here would replace real state with a fresh secret.
func TestFromMarker_NotAMarker(t *testing.T) {
	for _, v := range []any{
		"hunter2",
		map[string]any{"name": "default_admin"},
		[]any{1, 2},
		nil,
		42,
	} {
		if _, isMarker, err := FromMarker(v); isMarker || err != nil {
			t.Fatalf("FromMarker(%#v) = (marker=%v, err=%v), want plain data", v, isMarker, err)
		}
	}
}

// TestFromMarker_MalformedFailsClosed — a value that CLAIMS to be a request but does
// not parse is an error, never a default. Falling back to a default policy would mint a
// secret under rules nobody wrote.
func TestFromMarker_MalformedFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"payload not an object", map[string]any{MarkerKey: "please"}, "expected an object"},
		{"unknown policy key", map[string]any{MarkerKey: map[string]any{"lenght": 32}}, "unknown key"},
		{"length out of range", map[string]any{MarkerKey: map[string]any{"length": 2}}, "8..1024"},
		{"marker with siblings", map[string]any{MarkerKey: map[string]any{}, "name": "x"}, "only key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, isMarker, err := FromMarker(tc.in)
			if !isMarker {
				t.Fatal("not recognised as a marker — a malformed request must be reported, not treated as data")
			}
			if err == nil {
				t.Fatal("parsed without error, want a failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestParse_RejectsUnknownKeys — a misspelled key must not be silently ignored: it
// would hand out secrets from the DEFAULT alphabet while the YAML claims otherwise.
// All offenders are named at once, in sorted order (map iteration is unordered, and an
// error text that varies run to run on the same input is not diagnosable).
func TestParse_RejectsUnknownKeys(t *testing.T) {
	_, err := Parse(map[string]any{"charsett": "hex", "alowed_chars": "ab"}, Default())
	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	if !strings.Contains(err.Error(), "alowed_chars, charsett") {
		t.Fatalf("error %q does not name both offenders in sorted order", err)
	}
}

// TestParse_PresentButEmpty — an author who writes a key means something by it. Reading
// `charset: ""` or `allowed_chars: ""` as "absent" answers with the DEFAULT alphabet: the
// generator runs, the secret looks fine, and nothing anywhere says the policy was
// discarded. Both must refuse instead.
func TestParse_PresentButEmpty(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"empty charset":                 {"charset": ""},
		"empty allowed_chars":           {"allowed_chars": ""},
		"charset + empty allowed_chars": {"charset": CharsetHex, "allowed_chars": ""},
		"empty charset + allowed_chars": {"charset": "", "allowed_chars": "abcdef"},
	} {
		if _, err := Parse(m, Default()); err == nil {
			t.Errorf("%s: Parse accepted %v, want a refusal", name, m)
		}
	}
}
