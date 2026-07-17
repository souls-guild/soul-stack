package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// errHostDestroyed: the claimed host is in `destroyed` status (removed by a
// cloud-destroy cascade; the sole writer of `destroyed` is
// coremod/cloud.CascadeDestroy, an explicit terminal state, not a proxy for
// disconnect/revoke/reap). claim.execute treats it as a benign no_match, not a
// failure (NIM-56). The predicate is status-based, not tied to "this run".
var errHostDestroyed = errors.New("scenario: RenderForHost: host removed by cloud-destroy cascade of this run")

// RenderForHost reproduces the Keeper-side render pipeline for a run on claim
// (ADR-027, Phase 1.4.3): given a persisted [applyrun.Recipe] and the claimed
// host's SID, it follows the SAME path as the run-goroutine — load service →
// parse scenario → ExpandIncludes → essence.Resolve → ResolveInputValuesVault
// (SECRETS resolve HERE, in RAM) → Render (CEL+vault) — rendering the FULL
// run roster (like the run-goroutine), not a single host.
//
// Strategy Y (architect decision): full-roster render avoids a
// per-host/full-roster dialect. A single-host render (in.Hosts=[host]) would
// silently diverge from the old path — run_once wouldn't dedupe (the task
// would land on every host instead of one), soulprint.hosts/.where and
// incarnation.host_count would collapse to a one-host roster, cross-host
// state_changes.sets would lose neighbors. The set of full-roster dependencies
// is open-ended, so instead of ad hoc guards, the Acolyte reproduces the full
// roster EXACTLY like the old path and filters its own SID via
// groupByHost(tasks, plans)[sid] on the caller side.
//
// Cost of Y (ADR-027 trade-offs): each of N claims renders the full roster —
// O(N²) per-host CEL + N per-host vault resolves per run vs. O(N) on the old
// path. Acceptable in Phase 1 (tens of hosts); hundreds of hosts is an
// optimization candidate.
//
// loadHostFacts runs BEFORE the full-roster render to preserve the
// single-SID validation that the host is still in the roster (disconnected/
// revoked between dispatch and claim → error): the roster loads for render
// anyway, so the check is cheap and not lost.
//
// Invariant A (ADR-027): recipe.Input carries vault refs as STRINGS; secrets
// are resolved by ResolveInputValuesVault IN RAM (this call's stack) and never
// go back to the recipe/PG. Full-roster doesn't change invariant A: input
// secrets are scenario-level (resolved once, not per-host), so full-roster
// doesn't increase secret material in RAM. Returned tasks/plans live only in
// the Acolyte's memory until SendApply.
//
// applyID feeds the vault-resolve audit context (whose identity, which run
// read the secret; taken from the run row, symmetric with the run-goroutine
// path). Returns the flat []RenderedTask for the whole run + []DispatchPlan
// (caller filters by sid).
func RenderForHost(ctx context.Context, deps Deps, recipe *applyrun.Recipe, incarnationName, applyID, sid string) ([]*render.RenderedTask, []render.DispatchPlan, error) {
	if recipe == nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: nil recipe")
	}
	if sid == "" {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: empty sid")
	}

	// 1. Service artifact (git snapshot) + parse scenario/<name>/main.yml.
	art, err := deps.Loader.Load(ctx, recipe.ServiceRef)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: load service: %w", err)
	}
	// The Acolyte mirrors the run-goroutine path: an upgrade run loads
	// upgrade/<slug>/ (recipe.FromUpgrade), a regular run loads scenario/<name>/ (ADR-0068).
	scn, err := parseScenarioFromArtifact(deps.Loader, art, recipe.ScenarioName, recipe.FromUpgrade)
	if err != nil {
		return nil, nil, err
	}

	// 2. Expand includes into a flat list — BEFORE render (as in the run-goroutine).
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(deps.Loader, art, recipe.ScenarioName))
	if diag.HasErrors(idiags) {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: expanding include in %s/%s: %s",
			recipe.ScenarioName, scenarioMainFile, firstError(idiags))
	}
	scn.Tasks = expanded

	// Synthesize install steps from modules[] (ADR-065) — the Acolyte must
	// reproduce EXACTLY the run-goroutine's plan (same tasks, same plan_index),
	// or the synthesized step would go missing on the claim path and the
	// TaskEvent↔RenderedTask correlation would drift.
	scn.Tasks, _ = config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules)

	// 3. Run roster (as in the run-goroutine): the incarnation's whole roster.
	//    The "host still in roster" SID validation (disconnected/revoked between
	//    dispatch and claim → error) happens HERE, before the full-roster render
	//    — the roster loads anyway, so the check is cheap. render operates on the
	//    FULL roster (strategy Y); the Acolyte filters its own SID on output.
	hosts, err := loadRosterWithHost(ctx, deps.Topology, deps.DB, incarnationName, sid)
	if err != nil {
		return nil, nil, err
	}

	// 4. incarnation (for the spec.essence essence-override + IncarnationMeta).
	inc, err := incarnation.SelectByName(ctx, deps.DB, incarnationName)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: load incarnation %q: %w", incarnationName, err)
	}

	// 5. Essence (effective layer). The OS-family representative is the roster's
	//    first host, symmetric with the run-goroutine (run.go step 4: hosts[0]);
	//    per-host essence is a future extension.
	essenceMap, err := deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: essence: %w", err)
	}

	// 6. Effective input: merge defaults/required + scoped vault-ref resolve
	//    (SECRETS resolve HERE, in RAM — invariant A) + value validation.
	resolver := buildInputVaultResolver(ctx, deps.Vault, deps.Audit, depsLogger(deps), inputVaultAuditCtx{
		aid:         recipeAID(recipe),
		incarnation: incarnationName,
		scenario:    recipe.ScenarioName,
	}, deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, recipe.Input, resolver)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: input %s/%s: %w", incarnationName, recipe.ScenarioName, err)
	}

	// 7. Render: vault-resolve → CEL → on/where → []RenderedTask + []DispatchPlan.
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts, // FULL roster (strategy Y) — caller filters its own SID
		// State is the incarnation.state snapshot for `incarnation.state.<path>`
		// (ADR-009/010). The Acolyte (failover-claim) must reproduce EXACTLY the
		// same params as the run-goroutine: state commits only AFTER a
		// successful apply run, so inc.State loaded now == the run's pre-run
		// stateBefore — a read-only snapshot identical to the original. Without
		// this, the Acolyte would render `incarnation.state.*` as no-such-key
		// and diverge from the original.
		State: inc.State,
		Ctx:   ctx,
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(recipe.ScenarioName),
		),
	}
	if deps.Destiny != nil {
		renderIn.Destiny = deps.Destiny.resolverFor(art.Manifest)
	}
	tasks, plans, err := deps.Render.Render(ctx, renderIn)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: render %s/%s: %w", incarnationName, recipe.ScenarioName, err)
	}
	return tasks, plans, nil
}

// loadRosterWithHost resolves the incarnation's WHOLE roster (for the
// full-roster render, strategy Y) and validates the claimed sid is in it. A
// host that fell out of the roster between dispatch and claim
// (disconnected/revoked) is an error — there's nothing to render the task
// against. The single-SID validation stays before the render (the roster
// loads anyway, so the check is cheap, and losing the disconnected-host check
// is not acceptable).
//
// NIM-56: exception — a host in `destroyed` status (removed by a cloud-destroy
// cascade) is a benign no_match, not a failure. Any other roster dropout
// (disconnected/revoked/ErrSoulNotFound/read-fail) is a generic error (fail-closed).
func loadRosterWithHost(ctx context.Context, topo *topology.Resolver, db keepersoul.ExecQueryRower, incarnationName, sid string) ([]*topology.HostFacts, error) {
	hosts, err := topo.LoadIncarnationHosts(ctx, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("scenario: RenderForHost: topology: %w", err)
	}
	for _, h := range hosts {
		if h.SID == sid {
			return hosts, nil
		}
	}
	if s, serr := keepersoul.SelectBySID(ctx, db, sid); serr == nil && s.Status == keepersoul.StatusDestroyed {
		return nil, errHostDestroyed
	}
	return nil, fmt.Errorf("scenario: RenderForHost: host %q not in the roster of incarnation %q (disconnected since dispatch)", sid, incarnationName)
}

// recipeAID unwraps recipe.StartedByAID (*string) to "" on nil — the shape
// inputVaultAuditCtx.aid expects (empty → archon_aid column NULL).
func recipeAID(r *applyrun.Recipe) string {
	if r.StartedByAID == nil {
		return ""
	}
	return *r.StartedByAID
}

// depsLogger returns Deps.Logger, or a discard logger when nil (Deps.Logger is
// optional — NewRunner substitutes discard the same way).
func depsLogger(deps Deps) *slog.Logger {
	if deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return deps.Logger
}
