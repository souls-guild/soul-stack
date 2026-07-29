//go:build integration

// Integration guard for the essence layer pre-flight resolves (NIM-271).
//
// THE DIVERGENCE. [Runner.PreflightAssert] used to resolve essence against an
// incarnation it INVENTED from the request (`Name`/`Service`/`Spec={input}`) and
// against the first roster host, falling back to a zero-value host. Neither is
// the incarnation the run will use:
//
//   - essence's coven overlay (`essence/coven/<label>.yaml`) is keyed on the
//     COVENS the resolver is handed. run() hands it the incarnation's declared
//     `covens[]`; the synthetic incarnation has none, and the zero-value host
//     has none either, so the whole coven layer silently vanished from the
//     pre-flight view;
//   - `spec.essence` (the operator's per-incarnation override) lives on the real
//     row and was likewise absent from the synthetic one.
//
// An assert reading such a value therefore got a DIFFERENT answer at the gate
// than the same predicate gets at render, in both directions: a false 422 for a
// value that exists, or a quiet pass for one that does not.
//
// NIM-270 is what made this reachable and worth fixing rather than documenting:
// the gate now also runs on the explicit-run path, where the real incarnation
// EXISTS and there is no excuse for inventing one. On the create path there is
// still no row, and the os overlay there remains genuinely unknowable (no host
// has reported yet) — that part is inherent, not a defect, and is stated in the
// PreflightAssert contract.
package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// covenEssenceServiceRepo is a service whose ONLY source for `essence.tier` is
// the coven overlay — there is no default for it, so a resolver that misses the
// coven layer does not merely get a stale value, it gets no key at all. The
// day-2 scenario asserts on that value.
func covenEssenceServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: coven-essence-guard
state_schema_version: 1
description: essence coven overlay read from a pre-flight assert
state_schema:
  type: object
  properties: {}
`)
	write("essence/_default.yaml", `# tier is deliberately absent here: only the coven overlay defines it.
base_marker: default
`)
	write("essence/coven/tagged.yaml", `tier: gold
`)
	write("scenario/verify_tier/main.yml", `name: verify_tier
description: assert over an essence value that only the coven overlay provides
state_changes: {}
input:
  expect_tier:
    type: string
    default: gold
tasks:
  - name: Guard the effective tier
    assert:
      that:
        - "has(essence.tier) && essence.tier == input.expect_tier"
      message: "essence.tier does not match expect_tier"
  - name: Echo on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init coven-essence-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// seedIncarnationWithCovens seeds an incarnation carrying declared stable coven
// tags plus its roster. The covens are the point: they select the essence
// overlay, and they live on the incarnation, not on the hosts (ADR-008
// amendment 2026-07-17/NIM-124 — the two axes are separate).
func seedIncarnationWithCovens(t *testing.T, name string, covens []string, sids ...string) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "coven-essence-guard", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, Covens: covens,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationWithCovens: %v", err)
	}
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{name})
	}
}

// TestIntegration_PreflightAssert_ResolvesEssenceOfTheRealIncarnation — NIM-271.
// The incarnation declares `covens: [tagged]`, so `essence.tier` resolves to
// gold for this run. Pre-flight must see the same value the render will: with a
// synthetic incarnation it saw no coven overlay at all and the assert failed on
// a value that is plainly there.
func TestIntegration_PreflightAssert_ResolvesEssenceOfTheRealIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithCovens(t, "tiered", []string{"tagged"}, "a.example.com")
	gitURL := covenEssenceServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "tiered",
		ServiceRef:      artifact.ServiceRef{Name: "coven-essence-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_tier",
		Input:           map[string]any{"expect_tier": "gold"},
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: the coven essence overlay of the REAL incarnation must be visible, got %v", err)
	}
}

// TestIntegration_PreflightAssert_EssenceOnEmptyRosterUsesIncarnationCovens —
// the case where resolving the REAL incarnation actually changes the answer,
// and the reason the fix is a fix rather than a tidy-up.
//
// With members bound, `hosts[0].Coven` already carries the incarnation's tags
// (ADR-080 unions them into the roster query), so the coven overlay resolved
// correctly even from a synthetic incarnation — by coincidence of another
// decision. Take the members away and that coincidence goes with them: the old
// code fell back to a ZERO-VALUE host, whose covens are empty, so the overlay
// vanished and an `essence.tier` assert failed on a value the run would plainly
// have seen. run() has always used keeperEssenceInput(inc.Covens) here; the gate
// now uses the same.
//
// The incarnation exists and the plan consumes its roster, so this is reachable
// through the explicit-run path opened by NIM-270 — the run would go on to abort
// `no_hosts`, but the operator deserves to be told which of the two is wrong.
func TestIntegration_PreflightAssert_EssenceOnEmptyRosterUsesIncarnationCovens(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// Row with covens, deliberately WITHOUT members.
	seedIncarnationWithCovens(t, "tiered-empty", []string{"tagged"})
	gitURL := covenEssenceServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "tiered-empty",
		ServiceRef:      artifact.ServiceRef{Name: "coven-essence-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_tier",
		Input:           map[string]any{"expect_tier": "gold"},
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: with an empty roster the coven overlay must still come from the incarnation, got %v", err)
	}
}

// TestIntegration_PreflightAssert_EssenceAssertStillRejects — the other half:
// resolving the real incarnation's essence must not turn the gate into a
// rubber stamp. Same overlay, a value the operator did not ask for → the assert
// fails on its merits.
func TestIntegration_PreflightAssert_EssenceAssertStillRejects(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithCovens(t, "tiered-wrong", []string{"tagged"}, "a.example.com")
	gitURL := covenEssenceServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "tiered-wrong",
		ServiceRef:      artifact.ServiceRef{Name: "coven-essence-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_tier",
		Input:           map[string]any{"expect_tier": "silver"},
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("PreflightAssert: expect_tier=silver against tier=gold must fail, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
}
