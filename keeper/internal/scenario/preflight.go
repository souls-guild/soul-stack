package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
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

// PreflightAssert evaluates scenario assert-predicates ON THE REQUEST PATH,
// before anything mutates — the create handler runs it before the incarnation
// is committed, the run handler before the scenario goroutine starts (ADR-027
// amendment, pre-flight gate; ADR-009 amendment 2026-06-23 form A, extended to
// the run path 2026-07-28). Main use case: a topology mismatch (roster doesn't
// satisfy the scenario invariant) is rejected as 422 assert_failed instead of
// becoming an `error_locked` the operator has to unlock — the rejection moves
// from the async render phase to the synchronous request path.
//
// Contract:
//
//   - A ROSTER-READING ASSERT IS EVALUATED ONLY WHERE THE ROSTER IN FRONT OF US
//     IS THE ONE THE ASSERT IS ABOUT. Two situations fail that test, and in
//     both the assert ([config.AssertReadsRoster]) is deferred to the render
//     fail-safe rather than measured against a roster that is not its subject:
//
//     (1) THE INCARNATION ROW DOES NOT EXIST YET (the create path, NIM-235).
//     The original contract read the roster by the root Coven label — souls
//     carried `incarnation.name` in `souls.coven[]`, so a roster could pre-date
//     its incarnation. NIM-124 (ADR-008 amendment 2026-07-17, migration 099)
//     moved membership onto `incarnation_membership`, whose FK requires that
//     row, and pre-flight runs BEFORE `incarnation.Create`. The roster there is
//     not empty-by-circumstance but IMPOSSIBLE, so `size(soulprint.hosts) == N`
//     rejected EVERY create carrying a topology assert.
//
//     (2) THE PLAN BUILDS ITS OWN ROSTER ([planBuildsRoster], NIM-270). A
//     provision-from-zero run creates its hosts mid-run and Stratify puts the
//     roster consumers after the refresh boundary; the roster at request time
//     is not a smaller version of the run's roster, it is a different thing
//     entirely. Evaluating the assert against it is the same false 422 as (1),
//     which is why this shares its predicate with the no_hosts bypass in
//     [Runner.run] §3 rather than restating it.
//
//     Everything else DOES evaluate: an existing incarnation plus a plan that
//     consumes its roster is exactly the case the two-point amendment was
//     written for, and it is reached through the explicit-run path (bind
//     members, then run — ADR-008 amendment 2026-07-28/NIM-209).
//
//   - Asserts reading only input/essence/incarnation always evaluate: nothing
//     about the roster changes their verdict, so they keep their
//     422-before-mutation on both paths.
//
//   - effectiveInput merges defaults + required fields from the `input:`
//     schema (config.ResolveInputValues, no vault resolve: ADR-027 invariant
//     A — secrets aren't materialized on the request path; input was
//     already validated by ValidateInput upstream, so value validation here
//     is guaranteed to pass).
//
//   - essence is resolved from the SAME input run() step 4 would use for this
//     run, so an `essence.*` predicate cannot answer one way here and another
//     at render ([Runner.resolvePreflightEssence], NIM-271). On the create path
//     there is no row and no host, so the os overlay is genuinely unknowable —
//     a property of creating something that does not exist yet, not a gap to
//     paper over.
//
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
	scn, err := r.parseScenario(ctx, art, spec.ScenarioName, spec.FromUpgrade)
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

	// Is the roster in front of us the one a topology assert is about? Two ways
	// it is not: the incarnation row does not exist yet, so membership cannot
	// either (NIM-124) — or the plan provisions its own hosts, so the roster it
	// will be measured against is created by the run itself (NIM-270). Both
	// answer "empty" for reasons that have nothing to do with topology.
	//
	// The row is READ, not merely counted: when it exists it is also the source
	// of the essence layers below (NIM-271), so one lookup answers both.
	// `inc == nil` means "no row" — the create path.
	inc, err := r.preflightIncarnation(ctx, spec.IncarnationName)
	if err != nil {
		return err
	}
	rosterIsSubject := !planBuildsRoster(scn.Tasks)
	deferReason := "the plan builds its own roster mid-run, so the assert is about hosts that do not exist yet (NIM-270)"
	if rosterIsSubject && inc == nil {
		rosterIsSubject = false
		deferReason = "the incarnation does not exist yet, so it has no roster (NIM-124)"
	}

	var hosts []*topology.HostFacts
	if rosterIsSubject {
		hosts, err = r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
		if err != nil {
			return fmt.Errorf("preflight: roster %s: %w", spec.IncarnationName, err)
		}
	} else {
		// Drop the asserts that read the roster; keep the rest. Dropping (rather
		// than evaluating against a roster that is not the assert's subject) is
		// what keeps the gate honest: the deferred assert still runs at render,
		// where the run's own roster is real — for a provision-from-zero plan
		// that is the FIRST point it exists at all.
		kept, deferred := partitionRosterAsserts(scn.Tasks)
		if len(deferred) > 0 {
			r.logger.Info("scenario: pre-flight defers roster asserts to render",
				slog.String("incarnation", spec.IncarnationName),
				slog.String("scenario", spec.ScenarioName),
				slog.String("reason", deferReason),
				slog.Any("asserts", deferred))
		}
		scn.Tasks = kept
		if !hasAssertTask(scn.Tasks) {
			return nil
		}
	}

	// effectiveInput: defaults + required merged (vault-ref stays a string,
	// invariant A). ValidateInput already rejected bad input upstream, so this
	// shouldn't fail for a correct flow — but surface it as an internal error
	// (not assert_failed) rather than swallow it.
	effectiveInput, err := config.ResolveInputValues(scn.Input, spec.Input)
	if err != nil {
		return fmt.Errorf("preflight: input %s/%s: %w", spec.IncarnationName, spec.ScenarioName, err)
	}

	essenceMap, err := r.resolvePreflightEssence(art, spec, inc, hosts)
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
		// State — the same snapshot run() carries, so `incarnation.state.<path>`
		// resolves here to what it will resolve to there (NIM-403, NIM-404).
		//
		// Omitting it was not a missing value but a missing KEY: cel_render.go
		// binds `incarnation.state` only when State is non-nil, so every
		// state-reading assert judged a context the run would never see. The two
		// symptoms are worth naming because only one of them was loud — a bare
		// read raised `no such key: state` (surfaced as a 500), while a
		// has()-guarded read raised nothing and simply evaluated to false,
		// refusing the run with a confident 422 about a condition the
		// incarnation satisfied. Writing the predicate defensively made the
		// failure silent.
		//
		// nil on the create path is correct and stays: there is no row yet, so
		// there is no state to read (NIM-124). Same shape as the essence fix of
		// NIM-271 — pre-flight takes the input run() would, not a poorer one.
		State: incarnationState(inc),
	}
	return r.deps.Render.EvalAsserts(ctx, in)
}

// incarnationState is the state snapshot a pre-flight assert reads, or nil when
// there is no row. A nil-safe accessor rather than an inline branch: the whole
// defect was that this value silently defaulted to absent, so the place it comes
// from is worth naming.
func incarnationState(inc *incarnation.Incarnation) map[string]any {
	if inc == nil {
		return nil
	}
	return inc.State
}

// preflightIncarnation reads the incarnation the run is about, or nil when it
// has no row yet (the create path — the operator's request IS what creates it).
// A missing row is not an error here; every other failure is.
func (r *Runner) preflightIncarnation(ctx context.Context, name string) (*incarnation.Incarnation, error) {
	if r.deps.DB == nil {
		return nil, nil
	}
	inc, err := incarnation.SelectByName(ctx, r.deps.DB, name)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("preflight: select incarnation %s: %w", name, err)
	}
	return inc, nil
}

// resolvePreflightEssence resolves the essence layers an assert will read,
// choosing the SAME input run() step 4 would choose for this run (NIM-271):
//
//   - a real incarnation with a roster → [essenceInput] over the first host, so
//     the os overlay is the one the run will render against;
//   - a real incarnation with an empty roster → [keeperEssenceInput], the
//     keeper-context form: no representative host, so no os overlay, and the
//     coven overlay comes from the incarnation's own `covens[]`. Taking the
//     zero-value host instead would drop the coven layer entirely and make the
//     gate disagree with the render of the very same run;
//   - no row at all (create) → a synthetic incarnation built from the request.
//     The os overlay is genuinely unknowable there — no host has reported yet —
//     and that is a property of the create path, not a defect to work around.
//
// On the first branch the coven overlay survives for a second reason worth
// naming, because it is easy to break by accident: since ADR-080 the roster
// query unions each host's own `souls.coven[]` with the covens (and name) of
// every incarnation it belongs to, so `hosts[0].Coven` ALREADY carries this
// incarnation's declared tags. The essence a pre-flight assert sees therefore
// matches render's — verified, not assumed (preflight_essence_integration_test.go).
func (r *Runner) resolvePreflightEssence(
	art *artifact.ServiceArtifact,
	spec RunSpec,
	inc *incarnation.Incarnation,
	hosts []*topology.HostFacts,
) (map[string]any, error) {
	if inc == nil {
		inc = &incarnation.Incarnation{
			Name:    spec.IncarnationName,
			Service: art.Manifest.Name,
			Spec:    incarnationSpecFromInput(spec.Input),
		}
	}
	if len(hosts) > 0 {
		return r.deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	}
	return r.deps.Essence.Resolve(keeperEssenceInput(art.LocalDir, inc))
}

// partitionRosterAsserts splits an expanded task list into the tasks pre-flight
// may still evaluate and the NAMES of the assert tasks it must defer to render
// because they read the roster ([config.AssertReadsRoster]).
//
// Only assert tasks are ever dropped: everything else stays in place so the
// include-group decisions [render.Pipeline.EvalAsserts] makes over this list
// (group-drop by include-`when:`) are computed on the same shape as at render —
// dropping a whole group's non-assert members would change which groups the
// pass sees.
//
// The returned names are for the operator-facing log line; an unnamed assert is
// reported by its `that[0]`, since a nameless task would otherwise log as an
// empty string.
func partitionRosterAsserts(tasks []config.Task) (kept []config.Task, deferred []string) {
	kept = make([]config.Task, 0, len(tasks))
	for i := range tasks {
		if render.IsAssertTask(tasks[i]) && config.AssertReadsRoster(&tasks[i]) {
			deferred = append(deferred, assertLabel(tasks[i]))
			continue
		}
		kept = append(kept, tasks[i])
	}
	return kept, deferred
}

// assertLabel — the task's name, falling back to its first predicate when the
// author left `name:` off (asserts are frequently written unnamed inside an
// include branch).
func assertLabel(t config.Task) string {
	if t.Name != "" {
		return t.Name
	}
	if t.Assert != nil && len(t.Assert.That) > 0 {
		return t.Assert.That[0]
	}
	return "<unnamed assert>"
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
