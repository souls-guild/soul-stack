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
// MERGED input (input-only eval, config.EvalValidateRules). First failure →
// [ErrValidateFailed] (handler → 422 validation_failed). Order is strict:
// schema/required/type first, then validate invariants — `that` rules are
// written assuming correct types (input.port > 0 is meaningless if port isn't
// a number).
func ValidateInput(ctx context.Context, loader InputScenarioLoader, ref artifact.ServiceRef, scenarioName string, provided map[string]any) error {
	if loader == nil {
		// Without a loader, sync validation is impossible — do NOT silently skip
		// it (that was the original gap). Return an explicit config error; the
		// handler decides (in prod the loader is always wired up together with
		// the runner).
		return fmt.Errorf("scenario: validate input: loader is not configured")
	}

	art, err := loader.Load(ctx, ref)
	if err != nil {
		return fmt.Errorf("scenario: validate input: load service: %w", err)
	}

	rel := fmt.Sprintf(scenarioMainFile, scenarioName)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return fmt.Errorf("scenario: validate input: read %s: %w", rel, err)
	}
	// $type references in the input schema are resolved HERE (at load time) so
	// that config.ResolveInputValues below validates a submitted $type field
	// value against the RESOLVED type shape (object/array/properties/required),
	// instead of silently accepting it (a reference node has empty Type →
	// validation skipped).
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, rel, data)
	if err != nil {
		return fmt.Errorf("scenario: validate input: parse %s: %w", rel, err)
	}
	if diag.HasErrors(diags) {
		return fmt.Errorf("scenario: validate input: %s невалиден: %s", rel, firstError(diags))
	}

	// merge defaults + required + value validation (type/enum/pattern/length,
	// recursively into array/object). vault-ref isn't resolved (string pass-through).
	merged, verr := config.ResolveInputValues(scn.Input, provided)
	if verr != nil {
		return fmt.Errorf("%w: %v", ErrInputInvalid, verr)
	}

	// validate: — declarative input invariants over the merged input
	// (input-only CEL sandbox). no-op for scenarios without a validate section.
	fail, evErr := config.EvalValidateRules(scn.Validate, merged)
	if evErr != nil {
		// Compile/eval failure (nearly impossible after schema validation — the
		// config validator already compiled `that` input-only; non-bool `that`
		// is rejected at load) — an internal pre-flight failure (handler → 500),
		// NOT validation_failed.
		return fmt.Errorf("scenario: validate rules %s/%s: %w", scenarioName, rel, evErr)
	}
	if fail != nil {
		return fmt.Errorf("%w: %s", ErrValidateFailed, fail.Error())
	}
	return nil
}
