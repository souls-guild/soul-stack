package scenario

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ErrInputInvalid — sync validation of operator-provided input against the
// scenario's `input:` schema failed (a required field without a default was
// missing, type mismatch, pattern/enum/length violation). HTTP handler maps it
// to 422 `input_invalid`; MCP maps it to an analogous error.
//
// Historical gap (root cause of the "created an incarnation without required
// fields" bug): input value validation lived ONLY in the async run goroutine
// (run.go step 4.5, ResolveInputValuesVault → abort→error_locked). POST
// /v1/incarnations and POST .../scenarios/{scenario} returned 202 "accepted"
// BEFORE validation, and the real failure surfaced as
// incarnation.status=error_locked after the fact. This sentinel + [ValidateInput]
// close the gap: the check now runs sync BEFORE any mutation.
var ErrInputInvalid = errors.New("scenario: input invalid")

// ErrValidateFailed — a declarative rule in the top-level `validate:` section
// (ADR-009 amendment 2026-06-23, DSL wave 2) failed the request path's
// pre-flight gate: a scenario input invariant was violated (e.g. the
// cross-field precondition "port is required if tls is disabled"). HTTP
// handler maps it to 422 `validation_failed` (same class as input_invalid —
// input semantics don't check out; URN `validation-failed`), SEPARATE from
// ErrAssertFailed (assert is topology/roster, full context). No incarnation is
// created, no error_locked is set — the failure happens at the model stage
// BEFORE commit and BEFORE applying.
//
// A separate sentinel from ErrInputInvalid: both → 422, but distinguishable
// for the handler (different detail text: "input doesn't match the schema" vs
// "scenario input invariant violated"). validate: SUPPLEMENTS the input schema
// and required_when, doesn't replace them (input-only eval,
// config.EvalValidateRules).
var ErrValidateFailed = errors.New("scenario: validate rule failed")

// InputGate is what the pre-flight pass of [ValidateInput] produced beyond "it is
// valid": the EFFECTIVE input (defaults merged, vault-refs still strings) and the
// scenario's `id:` block. [ResolveCreatePlan] reads both without a second snapshot
// load.
type InputGate struct {
	Merged map[string]any
	ID     config.IDSpec
	// RosterField is the `input:` field the scenario declares as its roster
	// (`source: { roster: true }`, NIM-371), empty when it declares none. Read by
	// [ResolveCreatePlan] to hand the create path the SIDs it must bind into
	// `incarnation_membership` before the bootstrap run, without a second
	// snapshot load/parse.
	RosterField string
}

// InputScenarioLoader — the narrow [artifact.ServiceLoader] surface
// [ValidateInput] needs: materialize a service-ref snapshot and read
// scenario/<name>/main.yml. *artifact.ServiceLoader satisfies it; unit tests
// substitute a fake without the git stack.
type InputScenarioLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// ValidateInput synchronously checks operator-provided input against the
// `input:` schema of scenario `scenarioName` for service `ref` — BEFORE any
// mutation (insert incarnation / enqueue a run). Mirrors the "merge defaults +
// required + value validation" phase from the async run goroutine (run.go step
// 4.5), but moved to the request path so the operator gets 422 immediately
// instead of "created → error_locked" after the fact.
//
// Vault-ref resolution does NOT happen here (config.ResolveInputValues, not
// ...Vault): a `vault:...` secret field's value passes through as a string.
// Required + type + pattern/enum/length are checked in full; the final scoped
// vault resolve stays in the run goroutine (Invariant A, ADR-027 — secrets
// aren't materialized on the request path). This is fine: the required-field
// gap was about a missing field, not about secret contents.
//
// scn.Input is parsed directly from main.yml's top-level `input:` block —
// include expansion isn't needed (include brings in tasks, not the input
// schema). Snapshot load/parse errors are returned as-is (handler → 500/502);
// the value-validation error itself is wrapped in [ErrInputInvalid] (handler →
// 422).
//
// AFTER value validation succeeds, the same pass (no second snapshot load)
// evaluates the top-level `validate:` section's declarative rules over the
// MERGED input and the incarnation facts inc says this path knows
// (config.EvalValidateRules). First failure → [ErrValidateFailed] (handler → 422
// validation_failed). Order is strict: schema/required/type first, then validate
// invariants — `that` rules are written assuming correct types (input.port > 0 is
// meaningless if port isn't a number).
//
// inc is the caller's stance: [DayTwoIncarnation] on the run path,
// config.RequestedIncarnation on create. A rule reading a fact the stance does not
// carry is refused rather than evaluated against an empty namespace — the failure
// is wrapped in config.ErrValidateRuleEval, so the handler reports it as a
// scenario malfunction and not as the operator's input being wrong.
func ValidateInput(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName string, provided map[string]any, inc config.ValidateContext) (InputGate, error) {
	var zero InputGate
	scn, err := loadScenarioManifest(ctx, loader, ref, scenarioName, "validate input")
	if err != nil {
		return zero, err
	}

	// A create scenario that composes its own id (ADR-0079) has no identifier to
	// offer the rules: the id is a function of the input this gate is still
	// resolving, and it is composed after the gate returns (ResolveCreatePlan).
	// Only the manifest says so, so the withdrawal happens here rather than at the
	// call site, which has not read it.
	if scn.ID.Template != "" {
		inc = inc.WithComposedID()
	}

	// The full input gate in one call (config.ResolveInputContract, shared with
	// the destiny render pass): merge defaults + required/required_when + value
	// validation (type/enum/pattern/length, recursively into array/object), then
	// the `validate:` invariants over the merged input. vault-ref isn't resolved
	// (string pass-through).
	merged, err := config.ResolveInputContract(scn.Input, scn.Validate, provided, inc)
	if err != nil {
		var fail *config.ValidateRuleFailure
		switch {
		case errors.As(err, &fail):
			return zero, fmt.Errorf("%w: %s", ErrValidateFailed, fail.Error())
		case errors.Is(err, config.ErrValidateRuleEval):
			// Compile/eval failure (nearly impossible after schema validation — the
			// config validator already compiled `that` input-only; non-bool `that`
			// is rejected at load) — an internal pre-flight failure (handler → 500),
			// NOT validation_failed.
			return zero, fmt.Errorf("scenario: validate rules %s/%s: %w",
				scenarioName, fmt.Sprintf(scenarioMainFile, scenarioName), err)
		default:
			return zero, fmt.Errorf("%w: %v", ErrInputInvalid, err)
		}
	}
	return InputGate{
		Merged:      merged,
		ID:          scn.ID,
		RosterField: config.RosterInputField(scn.Input),
	}, nil
}

// loadScenarioManifest materializes the service snapshot and parses
// scenario/<name>/main.yml into its typed manifest (covenant merged, $type
// references resolved). op names the calling phase so the wrapped error reads the
// same as before the extraction ("validate input" / "preview name").
//
// Extracted from [ValidateInput] for [PreviewID]: the live name preview must
// read the SAME effective `input:` schema and the SAME `id:` block the create
// path validates against. A second load path here is exactly how the preview would
// start composing over a different contract than the create — the divergence class
// this whole feature exists to avoid.
//
// $type references in the input schema are resolved HERE (at load time) so that
// value validation downstream sees the RESOLVED type shape (object/array/
// properties/required) instead of silently accepting a reference node (empty Type
// → validation skipped).
//
// No plugin-manifest resolver (NIM-228): this entry point is package-level and
// carries no Deps, and it answers about the submitted INPUT rather than the task
// bodies. The same file is parsed with a resolver on the paths that act on those
// tasks — run and pre-flight — so nothing goes unchecked; a second
// resolve here would only cost a read per call.
func loadScenarioManifest(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName, op string) (*config.ScenarioManifest, error) {
	if loader == nil {
		// Without a loader, sync validation is impossible — do NOT silently skip
		// it (that was the original gap). Return an explicit config error; the
		// handler decides (in prod the loader is always wired up together with
		// the runner).
		return nil, fmt.Errorf("scenario: %s: loader is not configured", op)
	}

	art, err := loader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("scenario: %s: load service: %w", op, err)
	}

	rel := fmt.Sprintf(scenarioMainFile, scenarioName)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return nil, fmt.Errorf("scenario: %s: read %s: %w", op, rel, err)
	}
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, rel, data, nil)
	if err != nil {
		return nil, fmt.Errorf("scenario: %s: parse %s: %w", op, rel, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("scenario: %s: %s is invalid: %s", op, rel, firstError(diags))
	}
	return scn, nil
}
