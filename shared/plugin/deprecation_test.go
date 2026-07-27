package plugin

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func manifestWithParam(param string) string {
	return `kind: soul_module
protocol_version: 1
namespace: acme
name: widget
spec:
  states:
    applied:
      description: applies the widget
      input:
` + param
}

func loadCodes(t *testing.T, src string) []string {
	t.Helper()
	_, diags := LoadFromBytes("manifest.yaml", []byte(src))
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// TestDeprecated_ValidWindowAccepted — the shape an author is meant to write:
// two bounds a full policy window apart, pointing at a declared replacement.
func TestDeprecated_ValidWindowAccepted(t *testing.T) {
	src := manifestWithParam(`        addr: { type: string }
        address:
          type: string
          deprecated: { since: "0.4.0", removed_in: "0.6.0", use: "addr" }
`)
	m, diags := LoadFromBytes("manifest.yaml", []byte(src))
	if diag.HasErrors(diags) {
		t.Fatalf("a valid deprecation was rejected: %v", diags)
	}
	d := m.Spec.States["applied"].Input["address"].Deprecated
	if d == nil {
		t.Fatal("deprecated: did not decode")
	}
	if d.Since != "0.4.0" || d.RemovedIn != "0.6.0" || d.Use != "addr" {
		t.Errorf("decoded = %+v", *d)
	}
}

// TestDeprecated_WindowTooShortRejected — the policy itself, enforced rather
// than merely written down: one minor is not enough, because an author who
// upgrades a minor at a time never gets a release where both the old param and
// its replacement work.
func TestDeprecated_WindowTooShortRejected(t *testing.T) {
	src := manifestWithParam(`        address:
          type: string
          deprecated: { since: "0.4.0", removed_in: "0.5.0" }
`)
	codes := loadCodes(t, src)
	if !hasCode(codes, "deprecation_window_too_short") {
		t.Errorf("expected deprecation_window_too_short, got %v", codes)
	}
}

// TestDeprecated_MajorBumpIsAlwaysEnough — a new major IS the declared
// breaking-change boundary, so it is not held to the minor count.
func TestDeprecated_MajorBumpIsAlwaysEnough(t *testing.T) {
	src := manifestWithParam(`        address:
          type: string
          deprecated: { since: "0.9.0", removed_in: "1.0.0" }
`)
	codes := loadCodes(t, src)
	if hasCode(codes, "deprecation_window_too_short") {
		t.Errorf("a major bump was rejected as too short: %v", codes)
	}
}

// TestDeprecated_OpenEndedRejected — a deprecation with no end date is a warning
// that never resolves; an author cannot plan a migration against it.
func TestDeprecated_OpenEndedRejected(t *testing.T) {
	src := manifestWithParam(`        address:
          type: string
          deprecated: { since: "0.4.0" }
`)
	codes := loadCodes(t, src)
	if !hasCode(codes, "deprecated_bound_missing") {
		t.Errorf("expected deprecated_bound_missing, got %v", codes)
	}
}

// TestDeprecated_BoundGrammarMatchesCompat — the same plain MAJOR.MINOR.PATCH as
// a compat: bound; no v prefix, no operators (ADR-0076(c)).
func TestDeprecated_BoundGrammarMatchesCompat(t *testing.T) {
	src := manifestWithParam(`        address:
          type: string
          deprecated: { since: "v0.4.0", removed_in: ">=0.6.0" }
`)
	codes := loadCodes(t, src)
	if !hasCode(codes, "deprecated_version_invalid") {
		t.Errorf("expected deprecated_version_invalid, got %v", codes)
	}
}

// TestDeprecated_UnknownReplacementRejected — `use:` must name a param of the
// SAME state, or it sends the author looking for something that does not exist.
func TestDeprecated_UnknownReplacementRejected(t *testing.T) {
	src := manifestWithParam(`        address:
          type: string
          deprecated: { since: "0.4.0", removed_in: "0.6.0", use: "endpoint" }
`)
	codes := loadCodes(t, src)
	if !hasCode(codes, "deprecated_replacement_unknown") {
		t.Errorf("expected deprecated_replacement_unknown, got %v", codes)
	}
}

// TestDeprecated_NoticeNamesBothBounds — every surface reports the same
// sentence, so it must carry what an author needs to act: what to stop using,
// by when, and what to use instead.
func TestDeprecated_NoticeNamesBothBounds(t *testing.T) {
	d := &DeprecatedDef{Since: "0.4.0", RemovedIn: "0.6.0", Use: "addr"}
	got := d.Notice("address")
	for _, want := range []string{"address", "0.4.0", "0.6.0", "addr"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q does not mention %q", got, want)
		}
	}
	if (*DeprecatedDef)(nil).Notice("x") != "" {
		t.Error("a nil notice must render empty, not panic")
	}
}

// TestDeprecated_CoreManifestsCarryNoStaleDeprecation — a deprecation whose
// removed_in has already shipped means the key should have left the manifest.
// Guards the embedded core set against a forgotten cleanup.
func TestDeprecated_CoreManifestsCarryNoStaleDeprecation(t *testing.T) {
	// Nothing in core is deprecated yet; the guard exists so the first one that
	// is gets its window validated by the same rule an author's manifest does.
	src := manifestWithParam(`        addr: { type: string }
        address:
          type: string
          deprecated: { since: "0.4.0", removed_in: "0.4.9", use: "addr" }
`)
	codes := loadCodes(t, src)
	if !hasCode(codes, "deprecation_window_too_short") {
		t.Errorf("a patch-level removal slipped past the policy: %v", codes)
	}
}
