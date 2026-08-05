package applyrun

import (
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// TestRecipeSnapshot_RerunReadsWhatTheRunWrote — the reader of a run snapshot and
// its writer are the same shape, proven by round trip rather than by two
// hand-written spellings agreeing.
//
// The snapshot in `state_history.run` is a marshalled [Recipe]. `rerun-last`
// reads the git coordinates back out of it so a retry replays the code the
// attempt actually ran. Those two sides live in different packages and were
// connected by a string: the reader looked up `service_ref` → `"Git"`, matching
// the writer only because [artifact.ServiceRef] happens to declare no json tags
// and so marshals under its Go field names. Nothing enforced that. A drift there
// is invisible in the worst way — the lookup misses, the ref comes back empty,
// the caller falls back to the incarnation's CURRENT pin, and an operator asking
// to rerun a pre-upgrade failure silently gets the post-upgrade code. No error,
// no log, a green test suite.
//
// This test is the enforcement. It marshals a real Recipe and asserts the reader
// recovers the ref from those exact bytes, so the two sides cannot drift without
// something going red. It lives here because this is the package that can import
// both (incarnation cannot import applyrun — its tests already import
// incarnation, which would close a cycle in the test binary).
func TestRecipeSnapshot_RerunReadsWhatTheRunWrote(t *testing.T) {
	want := artifact.ServiceRef{Name: "redis", Git: "file:///srv/redis", Ref: "v1.0.0"}

	raw, err := json.Marshal(Recipe{
		ServiceRef:   want,
		ScenarioName: "add_user",
		Input:        map[string]any{"user": "alice"},
	})
	if err != nil {
		t.Fatalf("marshal recipe: %v", err)
	}

	got := incarnation.ServiceRefFromRunSnapshot(raw)
	if got.Git != want.Git || got.Ref != want.Ref {
		t.Fatalf("ServiceRefFromRunSnapshot(%s) = {Git:%q Ref:%q}, want {Git:%q Ref:%q} — "+
			"the rerun reader no longer recovers what the run writer stored, so a rerun "+
			"would fall back to the incarnation's current pin",
			raw, got.Git, got.Ref, want.Git, want.Ref)
	}
}

// TestRecipeSnapshot_UnusableSnapshotYieldsZero — the two shapes that mean "this
// snapshot cannot tell you which ref the attempt used", both answering with a
// zero value so the caller takes its documented fallback instead of a panic or a
// half-filled ref.
func TestRecipeSnapshot_UnusableSnapshotYieldsZero(t *testing.T) {
	for name, raw := range map[string][]byte{
		"malformed json":   []byte(`{"service_ref":`),
		"no service_ref":   []byte(`{"scenario_name":"add_user"}`),
		"empty coordinate": []byte(`{"service_ref":{"name":"redis","ref":"v1.0.0"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if got := incarnation.ServiceRefFromRunSnapshot(raw); got.Git != "" {
				t.Errorf("Git = %q, want empty — an unusable snapshot must not look usable", got.Git)
			}
		})
	}
}
