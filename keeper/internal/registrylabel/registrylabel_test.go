package registrylabel

// The behaviour of [Normalize] and [Display], and — separately, and at the end —
// THE STAKES: an executable statement of what a caption in a derived address
// would actually cost, which is the premise every per-surface guard rests on.
//
// The per-surface guards live next to their derivations and are listed in the
// package doc. This file does not repeat them; it establishes why they exist.

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestNormalize(t *testing.T) {
	str := func(s string) *string { return &s }

	cases := []struct {
		name string
		in   *string
		want *string
		why  string
	}{
		{"nil stays nil", nil, nil, "a caller that sent nothing gets nothing back"},
		{"empty becomes nil", str(""), nil,
			"\"no caption\" and \"a caption blanked out\" are one state, so the consumer's fallback has one trigger"},
		{"blank becomes nil", str("   \t\n "), nil, "whitespace is not a caption"},
		{"surrounding whitespace is trimmed", str("  Redis — Billing  "), str("Redis — Billing"),
			"a stray space is not part of what an operator meant to write"},
		{"capitals survive", str("Redis"), str("Redis"),
			"the capital letter lives in label and never in id (ADR-0085)"},
		{"inner punctuation and spacing survive", str("Redis — Billing (prod) / EU"), str("Redis — Billing (prod) / EU"),
			"free text is the point; only the identifier has a grammar"},
		{"a long caption is not truncated", str(strings.Repeat("x", 500)), str(strings.Repeat("x", 500)),
			"no length bound is invented here — synods.description carries none either"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("Normalize(%q) = %q, want nil — %s", derefOr(tc.in, "<nil>"), *got, tc.why)
			case tc.want != nil && got == nil:
				t.Errorf("Normalize(%q) = nil, want %q — %s", derefOr(tc.in, "<nil>"), *tc.want, tc.why)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("Normalize(%q) = %q, want %q — %s", derefOr(tc.in, "<nil>"), *got, *tc.want, tc.why)
			}
		})
	}
}

// TestNormalize_DoesNotAliasItsInput pins that the returned pointer is safe to
// store: a caller that mutates the string it passed in must not silently rewrite
// what a row now holds.
func TestNormalize_DoesNotAliasItsInput(t *testing.T) {
	in := "  Redis  "
	got := Normalize(&in)
	if got == nil {
		t.Fatal("Normalize returned nil for a non-blank caption")
	}
	if got == &in {
		t.Error("Normalize returned the caller's own pointer; a later write through it would " +
			"change the stored caption behind the row's back")
	}
	in = "something else"
	if *got != "Redis" {
		t.Errorf("the normalized caption followed a write to the caller's variable: %q", *got)
	}
}

func TestDisplay(t *testing.T) {
	str := func(s string) *string { return &s }

	cases := []struct {
		name  string
		label *string
		id    string
		want  string
	}{
		{"a caption is shown", str("Redis — Billing"), "redis-billing", "Redis — Billing"},
		{"no caption falls back to the identifier", nil, "redis-billing", "redis-billing"},
		{"a blank caption falls back too", str("   "), "redis-billing", "redis-billing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Display(tc.label, tc.id); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheStakes_ACaptionInAVaultPathIsASilentOrphan is the premise every
// per-surface guard rests on, made executable.
//
// It does NOT test the invariant — no production code is involved in choosing
// the segment here. It demonstrates the COST of breaking it, using the real
// derivation, so that "a caption must not reach a Vault path" is a checkable
// statement rather than an assertion in a comment:
//
//  1. a caption that is itself a well-formed segment derives a path SUCCESSFULLY —
//     nothing errors, nothing warns, and the result is indistinguishable from a
//     correct path by inspection;
//  2. that path is DIFFERENT from the one the identifier derives;
//  3. so the value written under the identifier's path is simply not at the
//     caption's, and `secretwrite` writes with `Put` rather than `Patch` — the
//     old value is not merged into the new location, and nothing migrates it.
//
// Which is the whole argument in [ADR-0085] "Why `id` is immutable", clause 1:
// the failure mode is not an error, it is silence.
func TestTheStakes_ACaptionInAVaultPathIsASilentOrphan(t *testing.T) {
	const (
		identifier = "redis-billing"
		caption    = "redis-billing-prod" // a valid segment, deliberately
		service    = "redis"
		mount      = "secret"
	)
	field := config.SecretField{State: "password"}

	fromID, err := field.VaultPath(mount, service, identifier, "")
	if err != nil {
		t.Fatalf("deriving from the identifier failed: %v", err)
	}

	fromCaption, err := field.VaultPath(mount, service, caption, "")
	if err != nil {
		t.Fatalf("deriving from the caption failed: %v — the fixture must use a caption that is "+
			"a WELL-FORMED segment, or this test proves the wrong thing: the danger is a path "+
			"that succeeds and is wrong, not one that is rejected", err)
	}

	if fromID == fromCaption {
		t.Fatal("the fixture caption and identifier derive the same path; they must differ " +
			"for the demonstration to mean anything")
	}
	if !strings.Contains(fromID, "/"+identifier+"/") {
		t.Errorf("the identifier-derived path %q does not contain the identifier", fromID)
	}

	t.Logf("identifier → %s", fromID)
	t.Logf("caption    → %s  (well-formed, accepted, and holding nothing)", fromCaption)
}

// TestTheStakes_CaseAloneMovesAPath is the same argument at its narrowest, and
// the reason the identifier grammar is all-lower-case rather than merely
// "consistent": `ValidVaultPathSegment` allows capitals and nothing folds case,
// so `Redis` and `redis` are two different secrets. A caption differing from its
// identifier ONLY in capitalisation — the most likely caption an operator
// writes — would still relocate the value.
func TestTheStakes_CaseAloneMovesAPath(t *testing.T) {
	field := config.SecretField{State: "password"}

	lower, err := field.VaultPath("secret", "redis", "redis-billing", "")
	if err != nil {
		t.Fatalf("lower-case derivation failed: %v", err)
	}
	capitalised, err := field.VaultPath("secret", "redis", "Redis-Billing", "")
	if err != nil {
		t.Fatalf("capitalised derivation failed: %v — capitals pass ValidVaultPathSegment, "+
			"which is exactly the point of this test", err)
	}
	if lower == capitalised {
		t.Error("a case-only change produced the same path; if that were true the whole " +
			"lower-case rule for identifiers would be cosmetic, and it is not")
	}
}

func derefOr(p *string, fallback string) string {
	if p == nil {
		return fallback
	}
	return *p
}
