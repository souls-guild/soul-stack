//go:build integration

// Integration guard for the service-var layer a pre-flight assert reads
// (NIM-271, reshaped by ADR-0082 / NIM-412).
//
// THE DIVERGENCE. [Runner.PreflightAssert] used to resolve this layer against an
// incarnation it INVENTED from the request (`Name`/`Service`/`Spec={input}`) and
// against the first roster host, falling back to a zero-value host. Neither was
// the incarnation the run would use, so an assert got a DIFFERENT answer at the
// gate than the same predicate gets at render — a false 422 for a value that
// exists, or a quiet pass for one that does not.
//
// Half of that divergence is now structurally impossible: since ADR-0082 a
// service's vars are HOST-INVARIANT (the old `os/` and `coven/` overlay
// layers are gone), so the roster cannot make the gate and the render disagree
// no matter which host — or no host — is in front of it. The tests below pin
// that as a property rather than assuming it: the same assert passes with a
// roster, with an empty roster, and with an incarnation carrying no covens at
// all, because none of the three is an input to the answer any more.
//
// What survives from NIM-270/271 is the other half: on the create path there is
// still no row, and the gate resolves against a synthetic incarnation built from
// the request. That is inherent, and stated in the PreflightAssert contract.
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

// varsAssertServiceRepo is a service whose ONLY source for `vars.tier` is its
// own `vars/`, so a resolver that reads the wrong place does not merely get a
// stale value — it gets no key at all, and `has(vars.tier)` in the day-2
// scenario's assert says so.
func varsAssertServiceRepo(t *testing.T) string {
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
	write("service.yml", `name: service-vars-guard
state_schema_version: 1
description: a service var read from a pre-flight assert
state_schema:
  type: object
  properties: {}
`)
	write("vars/00-base.yaml", `base_marker: default
tier: gold
`)
	// The mechanism that replaced the hard-wired `essence/coven/<label>.yaml`
	// overlay. Nothing else in this repo defines `tier`, so a resolve that misses
	// this step does not get a stale value — it gets a DIFFERENT one, and the
	// assert says which.
	write("vars/_stack.yaml", `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    optional: true
`)
	write("vars/coven/tagged.yaml", `tier: platinum
`)
	write("scenario/verify_tier/main.yml", `name: verify_tier
description: assert over a value only the service's vars provide
state_changes: {}
input:
  expect_tier:
    type: string
    default: gold
tasks:
  - name: Guard the effective tier
    assert:
      that:
        - "has(vars.tier) && vars.tier == input.expect_tier"
      message: "vars.tier does not match expect_tier"
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
	if _, err := wt.Commit("init service-vars-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// seedIncarnationWithCovens seeds an incarnation carrying declared stable coven
// tags plus its roster. The tags are load-bearing: the fixture's `_stack.yaml`
// fans out over them, so they decide whether `tier` is gold or platinum.
func seedIncarnationWithCovens(t *testing.T, name string, covens []string, sids ...string) {
	t.Helper()
	inc := &incarnation.Incarnation{
		Name: name, Service: "service-vars-guard", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, Covens: covens,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationWithCovens: %v", err)
	}
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{name})
	}
}

func preflightTier(t *testing.T, gitURL, incName, expect string) error {
	t.Helper()
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)
	return r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: incName,
		ServiceRef:      artifact.ServiceRef{Name: "service-vars-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_tier",
		Input:           map[string]any{"expect_tier": expect},
		StartedByAID:    "archon-alice",
	})
}

// TestIntegration_PreflightAssert_ResolvesServiceVarsOfTheRealIncarnation —
// NIM-271. The gate must see the layer the render will see.
func TestIntegration_PreflightAssert_ResolvesServiceVarsOfTheRealIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// No covens: this test is about the gate reading the REAL row, not about the
	// overlay. With `tagged` the stack step would load coven/tagged.yaml and tier
	// would be platinum — which is what the coven test below asserts.
	seedIncarnationWithCovens(t, "tiered", nil, "a.example.com")

	if err := preflightTier(t, varsAssertServiceRepo(t), "tiered", "gold"); err != nil {
		t.Fatalf("PreflightAssert: the service's own vars must be visible at the gate, got %v", err)
	}
}

// TestIntegration_PreflightAssert_ServiceVarsOnEmptyRoster — the roster shape
// that used to change the answer. The old code fell back to a ZERO-VALUE host
// whose covens are empty, so the overlay vanished and the assert failed on a
// value the run would plainly have seen. There is no overlay to lose now, and
// this holds the resolve to that: same repo, no members, same verdict.
//
// The incarnation exists and the plan consumes its roster, so this is reachable
// through the explicit-run path opened by NIM-270 — the run would go on to abort
// `no_hosts`, but the operator deserves to be told which of the two is wrong.
func TestIntegration_PreflightAssert_ServiceVarsOnEmptyRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// Row WITHOUT members, and without covens — the subject here is the roster
	// shape, not the overlay.
	seedIncarnationWithCovens(t, "tiered-empty", nil)

	if err := preflightTier(t, varsAssertServiceRepo(t), "tiered-empty", "gold"); err != nil {
		t.Fatalf("PreflightAssert: an empty roster must not change a host-invariant layer, got %v", err)
	}
}

// TestIntegration_PreflightAssert_StackStepReadsIncarnationCovens — the
// replacement mechanism, end to end on live PG and through the RUN's own resolve
// input, not the resolver's unit fixtures.
//
// The incarnation declares `covens: [tagged]`, so the `foreach:` step loads
// `coven/tagged.yaml` and `tier` becomes platinum. The pairing with the untagged
// case below is what makes it a guard rather than a demonstration: drop the
// covens column from `selectIncarnationForUpdateSQL` (or from the resolve input)
// and exactly one of the two flips.
func TestIntegration_PreflightAssert_StackStepReadsIncarnationCovens(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithCovens(t, "tagged-inc", []string{"tagged"}, "a.example.com")

	gitURL := varsAssertServiceRepo(t)
	if err := preflightTier(t, gitURL, "tagged-inc", "platinum"); err != nil {
		t.Fatalf("PreflightAssert: the incarnation's coven must select its overlay, got %v", err)
	}
	if err := preflightTier(t, gitURL, "tagged-inc", "gold"); err == nil {
		t.Fatal("PreflightAssert: with the overlay applied, tier must no longer be gold")
	}
}

// TestIntegration_PreflightAssert_StackStepOnUntaggedIncarnation — the other
// half. No covens, so the `foreach:` iterates zero times, `optional:` never
// fires, and the base value stands.
func TestIntegration_PreflightAssert_StackStepOnUntaggedIncarnation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithCovens(t, "untagged", nil, "a.example.com")

	if err := preflightTier(t, varsAssertServiceRepo(t), "untagged", "gold"); err != nil {
		t.Fatalf("PreflightAssert: an untagged incarnation must keep the base value, got %v", err)
	}
}

// TestIntegration_PreflightAssert_CreatePathSeesRequestCovens — the create path,
// which is the ONE place the gate still has no row to read.
//
// It synthesises the incarnation from the request, and the request's covens must
// travel with it (`WithIncarnationLabels`). Without them the gate resolves zero
// coven layers while the create run — starting seconds later against the row it
// just inserted — resolves one: the NIM-271 divergence, reappearing on the only
// path that never had a row.
func TestIntegration_PreflightAssert_CreatePathSeesRequestCovens(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// Deliberately NO incarnation row: this is the create path.
	gitURL := varsAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	spec := RunSpec{
		IncarnationName: "not-yet-created",
		ServiceRef:      artifact.ServiceRef{Name: "service-vars-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_tier",
		Input:           map[string]any{"expect_tier": "platinum"},
		StartedByAID:    "archon-alice",
		Covens:          []string{"tagged"},
	}
	if err := r.PreflightAssert(context.Background(), spec); err != nil {
		t.Fatalf("PreflightAssert: the create request's covens must reach the synthetic incarnation, got %v", err)
	}

	// And they are genuinely doing the work: strip them and the same request
	// resolves the base value instead.
	spec.Covens = nil
	if err := r.PreflightAssert(context.Background(), spec); err == nil {
		t.Fatal("PreflightAssert: without the request's covens the overlay must not apply")
	}
}

// TestIntegration_PreflightAssert_ServiceVarAssertStillRejects — the other half:
// resolving the real incarnation's vars must not turn the gate into a rubber
// stamp. Same layer, a value the operator did not ask for → the assert fails on
// its merits.
func TestIntegration_PreflightAssert_ServiceVarAssertStillRejects(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationWithCovens(t, "tiered-wrong", nil, "a.example.com")

	err := preflightTier(t, varsAssertServiceRepo(t), "tiered-wrong", "silver")
	if err == nil {
		t.Fatal("PreflightAssert: expect_tier=silver against tier=gold must fail, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
}
