package scenario

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ErrAssertFailed re-exports the render sentinel (ADR-009 amendment
// 2026-06-23, two-point eval): pre-flight ([Runner.PreflightAssert]) and
// render-fail-safe ([render.Pipeline.EvalAsserts] / [render.Pipeline.Render])
// both return this ONE error, so the caller (create-handler) can distinguish
// "assert failed" (→ 422 assert_failed) from other pre-flight failures via
// [errors.Is]. No second sentinel: assert failure is one domain semantic
// shared by both points.
var ErrAssertFailed = render.ErrAssertFailed

// PreflightAssert evaluates scenario assert-predicates AT RUN CREATION (the
// create-handler's request path, before the incarnation is committed and
// before applying starts — ADR-027 amendment, pre-flight gate; ADR-009
// amendment 2026-06-23, form A). Main use case: a topology mismatch (roster
// doesn't satisfy the scenario invariant) is rejected as 422 assert_failed,
// with NO incarnation row and NO fail status (error_locked) — the rejection
// moves from the async render phase to the synchronous request path.
//
// Contract:
//   - roster resolves by the root Coven label spec.IncarnationName (req.Name
//     for create, ADR-008): [topology.Resolver.LoadIncarnationHosts] doesn't
//     require an existing incarnation row (rosterSQL filters by `coven[]`
//     souls; empty declared roles for a not-yet-created incarnation is not
//     an error). Roster-at-create = connected souls at creation time
//     (CreateTyped starts immediately → this is the roster of the imminent
//     run). 0 connected souls → a size==N topology assert legitimately
//     fails → ErrAssertFailed (correct: can't create+start an N-shard
//     cluster without N hosts).
//   - effectiveInput merges defaults + required fields from the `input:`
//     schema (config.ResolveInputValues, no vault resolve: ADR-027 invariant
//     A — secrets aren't materialized on the request path; input was
//     already validated by ValidateInput upstream, so value validation here
//     is guaranteed to pass).
//   - essence resolves for a representative host (mirrors run() step 4), so
//     an assert predicate referencing essence.* sees the same values as
//     render. No incarnation row exists yet for create, so a synthetic
//     Incarnation is built from spec (Name/Service/Spec=input) for the
//     essence override layer.
//   - EvalAsserts emits ONLY assert predicates (shared [render.evalAssertTask]
//     — same source as the render branch): first false → ErrAssertFailed.
//
// No-op for scenarios without assert tasks (the common case): EvalAsserts
// walks tasks, finds none, returns nil. Pre-flight loads its own snapshot
// (ValidateInput already loaded one too; the loader's shared snapshot cache
// makes the repeat Load cheap).
//
// Snapshot load / parse / roster / essence errors are NOT wrapped as
// ErrAssertFailed — the caller maps those to 500 (internal pre-flight
// failure), while ErrAssertFailed maps to 422 (model precondition not met).
func (r *Runner) PreflightAssert(ctx context.Context, spec RunSpec) error {
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		return fmt.Errorf("preflight: load service: %w", err)
	}
	scn, err := r.parseScenario(art, spec.ScenarioName, spec.FromUpgrade)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	// Expand includes BEFORE checking for assert tasks: in a dispatcher
	// scenario (redis main.yml), the top level is a mode-guard plus
	// `include:` branches, and the assert (size-guard) lives INSIDE the
	// included branch. Checking hasAssertTask on the unexpanded list would
	// miss it → false no-op (a real bug caught live: error_locked instead of
	// 422). Mirrors render.Pipeline, which also expands includes before
	// evaluating asserts on the expanded list — keeping a single source.
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, spec.ScenarioName))
	if diag.HasErrors(idiags) {
		return fmt.Errorf("preflight: include expansion in %s/%s: %s", spec.ScenarioName, scenarioMainFile, firstError(idiags))
	}
	scn.Tasks = expanded

	// Fast path: no assert tasks (the common case) — skip resolving
	// roster/essence/input for nothing.
	if !hasAssertTask(scn.Tasks) {
		return nil
	}

	hosts, err := r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
	if err != nil {
		return fmt.Errorf("preflight: roster %s: %w", spec.IncarnationName, err)
	}

	// effectiveInput: defaults + required merged (vault-ref stays a string,
	// invariant A). ValidateInput already rejected bad input upstream, so this
	// shouldn't fail for a correct flow — but surface it as an internal error
	// (not assert_failed) rather than swallow it.
	effectiveInput, err := config.ResolveInputValues(scn.Input, spec.Input)
	if err != nil {
		return fmt.Errorf("preflight: input %s/%s: %w", spec.IncarnationName, spec.ScenarioName, err)
	}

	// essence for the representative host (mirrors run() step 4). An empty
	// roster still fails the topology assert regardless; the essence layer is
	// empty (essence.* isn't used in pre-flight asserts today, but this keeps
	// symmetry with render).
	synthetic := &incarnation.Incarnation{
		Name:    spec.IncarnationName,
		Service: art.Manifest.Name,
		Spec:    incarnationSpecFromInput(spec.Input),
	}
	essenceMap, err := r.resolvePreflightEssence(art.LocalDir, synthetic, hosts)
	if err != nil {
		return fmt.Errorf("preflight: essence %s: %w", spec.IncarnationName, err)
	}

	in := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           spec.IncarnationName,
			Service:        spec.ServiceRef.Name,
			ServiceVersion: spec.ServiceRef.Ref,
		},
		Hosts: hosts,
	}
	return r.deps.Render.EvalAsserts(ctx, in)
}

// resolvePreflightEssence resolves essence for pre-flight using a
// representative host (first roster host, or synthetic empty at 0
// connected). Mirrors run() step 4 (essenceInput → Essence.Resolve) for the
// read-only pre-flight path.
func (r *Runner) resolvePreflightEssence(serviceDir string, inc *incarnation.Incarnation, hosts []*topology.HostFacts) (map[string]any, error) {
	host := &topology.HostFacts{}
	if len(hosts) > 0 {
		host = hosts[0]
	}
	return r.deps.Essence.Resolve(essenceInput(serviceDir, inc, host))
}

// hasAssertTask reports whether the flat task list contains at least one
// assert task. Called AFTER ExpandIncludes, since an assert may live at
// scenario top level or inside an include branch (redis main.yml dispatcher
// pattern), so the check runs against the already-expanded list. No eval
// duplication: actual assert evaluation is done by
// [render.Pipeline.EvalAsserts] — this predicate is just an early
// no-op check.
func hasAssertTask(tasks []config.Task) bool {
	for i := range tasks {
		if render.IsAssertTask(tasks[i]) {
			return true
		}
	}
	return false
}

// incarnationSpecFromInput builds the spec of a synthetic Incarnation for the
// pre-flight essence override layer: places operator input under key `input`
// (as CreateTyped does). essence reads its override from spec.essence, which
// is absent here (pre-flight at create only sees input, not an
// essence-override), so the override is empty; base essence resolves from
// the snapshot. nil input → empty spec.
func incarnationSpecFromInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	return map[string]any{"input": input}
}
