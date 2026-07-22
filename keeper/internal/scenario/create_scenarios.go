package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// ErrCreateScenarioNotEligible: the operator-chosen start scenario
// (`create_scenario` in POST /v1/incarnations) is not in the service's create
// set — invalid name, not flagged `create: true` (e.g. operational `add_user`),
// or missing from the snapshot. Handler maps it to 422 validation_failed: the
// incarnation is not created (rejected at the model stage).
var ErrCreateScenarioNotEligible = errors.New("scenario: chosen create_scenario is not an eligible bootstrap scenario for this service")

// ErrCreateScenarioRequired: the service HAS create scenarios (>=1 with
// `create: true`) but the operator chose none (`create_scenario` empty). A
// choice is mandatory — input validates against the CHOSEN scenario's
// `input:` schema, so an unchosen request has nothing to validate against.
// Handler maps it to 422 validation_failed listing eligible scenarios. Differs
// from [ErrCreateScenarioNotEligible] (there a choice WAS made but is
// ineligible) — here no choice was made against a non-empty set.
var ErrCreateScenarioRequired = errors.New("scenario: create_scenario is required (service offers create scenarios)")

// CreateScenarioLoader is the narrow [artifact.ServiceLoader] surface needed
// to resolve the create set: materialize the service-ref snapshot (its
// LocalDir is scanned via [artifact.ListScenarios]). *artifact.ServiceLoader
// satisfies it; unit tests substitute a fake without the git stack.
type CreateScenarioLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
}

// ResolveCreateScenarios returns the set of scenario names on service `ref`
// eligible as bootstrap scenarios for a new incarnation: EXACTLY those with
// top-level `create: true` in `scenario/<name>/main.yml` (supports multiple
// create scenarios). The name `create` is not privileged — it's only in the
// set if `scenario/create/main.yml` itself carries `create: true`.
//
// A service with no `create: true` at all yields an EMPTY set — a valid case:
// the caller treats it as a bare incarnation (StatusReady with no run, see
// [ValidateCreateScenarioChoice]); a non-empty choice against such a service
// is a 422.
//
// The snapshot loads through loader (loader-cached — a repeat load in the same
// request is a cache hit). Scenario-directory scanning reuses
// [artifact.ListScenarios] (same partial-success behavior: one scenario's
// broken YAML warns and is skipped, doesn't fail the whole set).
func ResolveCreateScenarios(ctx context.Context, loader CreateScenarioLoader, ref artifact.ServiceRef) (map[string]struct{}, error) {
	if loader == nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: loader is not configured")
	}
	art, err := loader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: load service: %w", err)
	}

	set := map[string]struct{}{}
	if art == nil {
		return set, nil
	}
	scenarios, err := artifact.ListScenarios(art.LocalDir, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: list %s: %w", ref.Name, err)
	}
	for _, sc := range scenarios {
		if sc.Create {
			set[sc.Name] = struct{}{}
		}
	}
	return set, nil
}

// ValidateCreateScenarioChoice resolves and validates the operator's chosen
// start scenario along three contract branches (decided 2026-06-29):
//
//   - chosen NON-EMPTY + in the create set → run it (name, bare=false); not in
//     the set / invalid name → [ErrCreateScenarioNotEligible].
//   - chosen EMPTY + set non-empty (service offers create scenarios) →
//     [ErrCreateScenarioRequired]: a choice is mandatory (input depends on scenario).
//   - chosen EMPTY + set EMPTY (no `create: true` at all) → bare incarnation
//     (returns "", bare=true): caller creates StatusReady with no run.
//
// Return is `(name, bare, err)`: bare=true always pairs with name="" — an
// unambiguous contract (no scenario name, no run); caller must branch on bare
// before interpreting name. Replaces the old back-compat shortcut (empty →
// default `create`).
//
// An invalid name (traversal/garbage per [ScenarioNamePattern]) is rejected as
// [ErrCreateScenarioNotEligible] before the set even resolves — garbage never
// reaches a path.
func ValidateCreateScenarioChoice(ctx context.Context, loader CreateScenarioLoader, ref artifact.ServiceRef, chosen string) (string, bool, error) {
	if chosen != "" && !ValidScenarioName(chosen) {
		return "", false, fmt.Errorf("%w: name %q does not match %s", ErrCreateScenarioNotEligible, chosen, ScenarioNamePattern)
	}
	set, err := ResolveCreateScenarios(ctx, loader, ref)
	if err != nil {
		return "", false, err
	}
	if chosen == "" {
		if len(set) == 0 {
			// No create scenarios → bare incarnation (no run).
			return "", true, nil
		}
		return "", false, fmt.Errorf("%w: choose one of %s", ErrCreateScenarioRequired, sortedNames(set))
	}
	if _, ok := set[chosen]; !ok {
		return "", false, fmt.Errorf("%w: %q", ErrCreateScenarioNotEligible, chosen)
	}
	return chosen, false, nil
}

// CreatePlanLoader is the narrow [artifact.ServiceLoader] surface needed by
// [ResolveCreatePlan]: combines [CreateScenarioLoader] (create-set resolve +
// lifecycle snapshot) and [InputScenarioLoader] (reading
// scenario/<name>/main.yml for input validation). *artifact.ServiceLoader
// satisfies it; unit tests substitute a fake without the git stack.
type CreatePlanLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// AssertPreflighter is the narrow scenario.Runner surface for the `assert:`
// pre-flight gate ([Runner.PreflightAssert], ADR-009/ADR-027 amendment
// 2026-06-23, form A). *Runner satisfies it; ScenarioStarter fakes lacking the
// method fail the type assertion in [ResolveCreatePlan], making the
// assert-gate a no-op (as it was in both handlers before). Duplicates the
// local handlers.AssertPreflighter / mcp.assertPreflighter interfaces — kept
// for package isolation (handlers/mcp don't pull the scenario-internal
// interface into their signature), but the actual gate lives here.
type AssertPreflighter interface {
	PreflightAssert(ctx context.Context, spec RunSpec) error
}

// CreatePlan is the result of [ResolveCreatePlan]: the resolved create start
// scenario plus branching flags, shared by REST CreateTyped and MCP
// callIncarnationCreate.
//
//   - CreateScenario is the actual bootstrap scenario (operator's choice, or
//     default [CreateScenarioName] in stub mode without a loader). Written to
//     incarnation.created_scenario (NULL when BareNoScenario, see handler).
//   - BareNoScenario: the service offers no `create: true` at all — the
//     incarnation is created StatusReady with NO run (created_scenario=NULL).
//   - AutoCreate is the target service's lifecycle.auto_create policy (default
//     true): false → incarnation ready with no run, but created_scenario is
//     non-empty (run deferred, not bare).
type CreatePlan struct {
	CreateScenario string
	BareNoScenario bool
	AutoCreate     bool
}

// ResolveCreatePlan is the shared resolve of the create start scenario, input
// validation, and the pre-flight assert gate for POST /v1/incarnations (REST
// CreateTyped) and keeper.incarnation.create (MCP). Extracted VERBATIM from
// both handlers (R2, behavior unchanged — same sentinel errors in the same
// order): duplicated branching removed, callers map returned errors to their
// own transport (*problemError / toolError) via errors.Is.
//
// Sequence (as in the original handlers):
//
//  1. loader == nil (REST stub mode: runner present, no loader) → plan skips
//     set resolution: {CreateScenarioName, bare=false, autoCreate=true} —
//     legacy "run `create`" behavior. MCP in production never hits this
//     (loader always accompanies the runner), but the contract stays symmetric.
//  2. loader != nil → [ValidateCreateScenarioChoice] (chosen in set / required /
//     bare). On bare, return immediately (no ValidateInput/lifecycle — no run).
//  3. non-bare → [ValidateInput] (required/type/validate against the CHOSEN
//     scenario's `input:` schema) + lifecycle.auto_create from the snapshot.
//  4. !bare && autoCreate → [AssertPreflighter.PreflightAssert] (no-op unless
//     preflighter implements the interface, as with a ScenarioStarter fake).
//
// Errors (for the caller's errors.Is): [ErrCreateScenarioRequired] /
// [ErrCreateScenarioNotEligible] / [ErrInputInvalid] / [ErrValidateFailed] /
// [ErrAssertFailed] are domain errors (422); everything else (snapshot
// load/parse, eval failure) is wrapped via fmt.Errorf (handler → 500).
func ResolveCreatePlan(
	ctx context.Context,
	loader CreatePlanLoader,
	preflighter any,
	incarnationName string,
	serviceRef artifact.ServiceRef,
	chosenScenario string,
	input map[string]any,
	startedByAID string,
) (CreatePlan, error) {
	// Default: stub mode / loader not configured — legacy `create`, not bare,
	// auto_create=true (as both handlers behaved with a nil loader).
	plan := CreatePlan{CreateScenario: CreateScenarioName, AutoCreate: true}

	if loader != nil {
		// Resolve+validate the start-scenario choice BEFORE ValidateInput: input
		// validates against the CHOSEN scenario's `input:` schema.
		chosen, isBare, err := ValidateCreateScenarioChoice(ctx, loader, serviceRef, chosenScenario)
		if err != nil {
			return CreatePlan{}, err
		}
		plan.CreateScenario = chosen
		plan.BareNoScenario = isBare

		// bare (no create scenario): skip ValidateInput / lifecycle resolve —
		// there's no run, and nothing to validate input against.
		if !isBare {
			if err := ValidateInput(ctx, loader, serviceRef, chosen, input); err != nil {
				return CreatePlan{}, err
			}
			art, err := loader.Load(ctx, serviceRef)
			if err != nil {
				return CreatePlan{}, fmt.Errorf("scenario: resolve create plan: load service snapshot: %w", err)
			}
			if art != nil && art.Manifest != nil {
				plan.AutoCreate = art.Manifest.Lifecycle.AutoCreateEnabled()
			}
		}
	}

	// Pre-flight assert gate (ADR-009/ADR-027 amendment 2026-06-23, form A):
	// AFTER ValidateInput (input materialized) and BEFORE incarnation.Create/Start.
	// Gated on !bare && autoCreate (bare has no scenario; autoCreate=false means
	// no run starts). Optional: a preflighter without PreflightAssert, or a
	// scenario without assert tasks, is a no-op. render-assert stays fail-safe
	// for TOCTOU.
	if !plan.BareNoScenario && plan.AutoCreate {
		if pf, ok := preflighter.(AssertPreflighter); ok {
			if err := pf.PreflightAssert(ctx, RunSpec{
				IncarnationName: incarnationName,
				ServiceRef:      serviceRef,
				ScenarioName:    plan.CreateScenario,
				Input:           input,
				StartedByAID:    startedByAID,
			}); err != nil {
				return CreatePlan{}, err
			}
		}
	}

	return plan, nil
}

// sortedNames returns a deterministic sorted name list for the
// [ErrCreateScenarioRequired] message (stable, testable 422 text).
func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
