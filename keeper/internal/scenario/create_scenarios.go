package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"unicode/utf8"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
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

// ErrIDNotComposable: the chosen create scenario composes the incarnation name
// from `id.template` (ADR-0079), but the request also carried an explicit
// `name`. Rejected rather than silently ignored or silently overridden: with a
// template the name is a function of the input components, and letting a request
// field disagree with the row that gets inserted would make the RBAC
// `incarnation=<name>` dimension checked on the request a different name than the
// one created. Handler → 422.
var ErrIDNotComposable = errors.New("scenario: create_scenario composes the incarnation id from id.template; the request must not carry `id`")

// ErrComposedIDInvalid: the name assembled from `id.template` does not match
// the incarnation name grammar — most often it overflows the 63-character ceiling
// once the operator's components are substituted. Deliberately NOT truncated: a
// silently shortened name is a different identity (the name is the immutable
// primary key). Handler → 422 with the offending name and its length, so the
// operator shortens a component. Distinct sentinel from
// [config.ErrIDTemplateRender] (there the template itself misfired).
var ErrComposedIDInvalid = errors.New("scenario: id composed from id.template is not a valid incarnation id")

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

// createOptions — what a caller may add to ResolveCreatePlan beyond its
// positional arguments. Variadic so the ~20 existing call sites (mostly tests
// that do not exercise the gate) stay as they are.
type createOptions struct {
	covens []string
	traits map[string]any
}

// CreateOption customises [ResolveCreatePlan].
type CreateOption func(*createOptions)

// WithIncarnationLabels supplies the covens and traits the request will write
// onto the incarnation row.
//
// Only the pre-flight gate reads them, and only on the create path: with no row
// to read it synthesises the incarnation, and a `vars/_stack.yaml` step keyed on
// `incarnation.covens` resolves against whatever it is handed. Omitting them
// makes the gate see zero coven layers where the create run a moment later sees
// however many the request declared — the NIM-271 divergence, on the one path
// that still has no row to resolve from.
func WithIncarnationLabels(covens []string, traits map[string]any) CreateOption {
	return func(o *createOptions) {
		o.covens = covens
		o.traits = traits
	}
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
//   - ComposedID is the incarnation name assembled from the chosen scenario's
//     `id.template` over the resolved input (ADR-0079). Empty when the scenario
//     declares no template — then the operator-supplied `name` stands, exactly as
//     before. Non-empty, it is already validated against the incarnation name
//     grammar and is THE name: the caller inserts it, targets the bootstrap run at
//     it and audits it.
type CreatePlan struct {
	CreateScenario string
	BareNoScenario bool
	AutoCreate     bool
	ComposedID     string
	// RosterField / RosterSIDs carry the roster the chosen create scenario declared
	// via `source: { roster: true }` (NIM-371): the field's name (for error text) and
	// the SIDs the operator picked, read off the EFFECTIVE input so a declared
	// `default:` is honoured like any other field.
	//
	// The caller binds these into `incarnation_membership` after the insert and
	// BEFORE starting the bootstrap run. That order is the whole point: a run
	// resolves its roster from that relation at start, so a scenario cannot bind
	// itself — it aborts `no_hosts` before its first task (see the header of
	// handlers/incarnation_members.go). Both empty when the scenario declares no
	// roster: then nothing is bound and the create path is unchanged.
	RosterField string
	RosterSIDs  []string
}

// EffectiveName is the name the incarnation is created under: the composed name
// when the create scenario declares a `id.template`, otherwise the
// operator-supplied requested name. Single source for both callers so REST and MCP
// cannot drift on which name wins.
func (p CreatePlan) EffectiveName(requested string) string {
	if p.ComposedID != "" {
		return p.ComposedID
	}
	return requested
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
//     scenario's `input:` schema) + name composition from `id.template` over the
//     resolved input (ADR-0079, [composeIncarnationID]) + lifecycle.auto_create
//     from the snapshot.
//  4. !bare && autoCreate → [AssertPreflighter.PreflightAssert] (no-op unless
//     preflighter implements the interface, as with a ScenarioStarter fake) — on
//     the COMPOSED name when there is one.
//
// incarnationName is the name the operator REQUESTED; with a `id.template` it
// must be empty and the effective name comes back in [CreatePlan.ComposedID]
// (see [CreatePlan.EffectiveName]).
//
// Errors (for the caller's errors.Is): [ErrCreateScenarioRequired] /
// [ErrCreateScenarioNotEligible] / [ErrInputInvalid] / [ErrValidateFailed] /
// [ErrAssertFailed] / [ErrIDNotComposable] / [ErrComposedIDInvalid] /
// [config.ErrIDTemplateRender] are domain errors (422); everything else (snapshot
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
	opts ...CreateOption,
) (CreatePlan, error) {
	var o createOptions
	for _, apply := range opts {
		apply(&o)
	}
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
			// The create path answers for the identity the request carried and
			// nothing else — the incarnation does not exist yet. A scenario with
			// `id.template` carries not even that, and ValidateInput withdraws it
			// there (only the manifest knows).
			gate, err := ValidateInput(ctx, loader, serviceRef, chosen, input,
				config.RequestedIncarnation(incarnationName))
			if err != nil {
				return CreatePlan{}, err
			}
			// Name composition (ADR-0079) sits HERE — after the input gate (the
			// template renders over the EFFECTIVE input, defaults merged) and before
			// the pre-flight assert, so the assert and the bootstrap run already see
			// the final name.
			if gate.ID.Template != "" {
				composed, cerr := composeIncarnationID(gate.ID, gate.Merged, incarnationName)
				if cerr != nil {
					return CreatePlan{}, cerr
				}
				plan.ComposedID = composed
				incarnationName = composed
			}
			// Roster (NIM-371) — read AFTER the input gate, off the merged values, so
			// the SIDs handed back are the ones the run will actually see. Shape
			// violations were already rejected by the gate (type/format/min_items),
			// which is why extraction here needs no second validation pass.
			if gate.RosterField != "" {
				plan.RosterField = gate.RosterField
				plan.RosterSIDs = rosterSIDsFromInput(gate.Merged[gate.RosterField])
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
				Covens:          o.covens,
				Traits:          o.traits,
			}); err != nil {
				return CreatePlan{}, err
			}
		}
	}

	return plan, nil
}

// rosterSIDsFromInput reads the roster field's value into a SID slice (NIM-371).
// Both declared shapes are accepted: an array of SIDs (the multi-select form) and a
// single SID string. JSON decoding gives `[]any` while a Go-side caller may pass
// `[]string`, so both are handled.
//
// Non-string elements and empty strings are DROPPED rather than reported: the input
// gate has already validated the field against its schema (`type`/`format: sid`/
// `min_items`) by the time this runs, so anything left here cannot be a legal SID,
// and a second error channel would only give the same rejection two spellings.
func rosterSIDsFromInput(v any) []string {
	switch val := v.(type) {
	case nil:
		return nil
	case string:
		if val == "" {
			return nil
		}
		return []string{val}
	case []string:
		out := make([]string, 0, len(val))
		for _, s := range val {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// composeIncarnationID is [ComposeID] behind one guard: requested MUST be empty
// ([ErrIDNotComposable]) — the id is derived, not negotiated.
func composeIncarnationID(spec config.IDSpec, merged map[string]any, requested string) (string, error) {
	if requested != "" {
		return "", fmt.Errorf("%w (composed from %q)", ErrIDNotComposable, spec.Template)
	}
	return ComposeID(spec, merged)
}

// ComposeID renders `id.template` over the resolved input and checks the result
// against the scenario's bounds and the incarnation id grammar (ADR-0079). The ONLY
// place either surface composes an id — a second implementation, client-side CEL above
// all, would compose a DIFFERENT string from the same input, and under an immutable
// primary key that is a different identity rather than a cosmetic mismatch. It is also
// the AUTHORITATIVE check of `id.max_length`, on the request path, before the insert.
//
// The composed string comes back on BOTH outcomes so a preview can show the offending
// value; callers acting on it branch on err, not on emptiness.
//
// THE GRAMMAR IS THE GATE, LENGTH ONLY EVER AN EXPLANATION, and length is counted in
// CHARACTERS: judging length first answers "too long" for a forbidden character, and
// `len` (bytes) would also push such a violation past the 63 gate — `namespace`
// declares no `pattern:` and 26 Cyrillic letters are 52 bytes.
func ComposeID(spec config.IDSpec, merged map[string]any) (string, error) {
	composed, err := config.RenderIDTemplate(spec.Template, merged)
	if err != nil {
		return "", err
	}
	length := utf8.RuneCountInString(composed)
	if !incarnation.ValidID(composed) {
		if length > config.IncarnationIDMaxLen {
			// The GUARD is the grammar's own 63 — that is what makes "too long" the
			// right attribution for a string the pattern rejected. The PHRASE is the
			// scenario's, because a string over 63 is over its narrower bound too, and
			// the number the operator has to fit is the binding one: telling them 63
			// while the preview beside it says 44 is the drift `id.max_length` exists
			// to remove. With no bound declared the two are the same sentence.
			return composed, fmt.Errorf("%w: %q is %d characters, over %s — shorten the input components feeding id.template",
				ErrComposedIDInvalid, composed, length, spec.CeilingPhrase())
		}
		return composed, fmt.Errorf("%w: %q does not match %s — check the input components feeding id.template",
			ErrComposedIDInvalid, composed, incarnation.IDPattern)
	}
	if length > spec.Ceiling() {
		return composed, fmt.Errorf("%w: %q is %d characters, over %s — shorten the input components feeding id.template",
			ErrComposedIDInvalid, composed, length, spec.CeilingPhrase())
	}
	return composed, nil
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
