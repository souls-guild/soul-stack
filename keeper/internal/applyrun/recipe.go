package applyrun

import (
	"encoding/json"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// Recipe is the render instruction for just-in-time rendering of a single
// host's task by the Acolyte at claim time (ADR-027(c)(f)). Persisted in the
// jsonb column `apply_runs.recipe` (migration 029) when a planned task is
// dispatched (Phase 1.4.2) and read back by the Acolyte at claim (Phase
// 1.4.3) to replay the vault-resolve → input-validation → CEL-render →
// text/template-render → ApplyRequest pipeline without relying on the
// run-goroutine's memory (which lives on a single instance and does not
// survive cross-Keeper routing / restart).
//
// Recipe is the persisted form of [scenario.RunSpec] minus ApplyID/
// IncarnationName (those are separate apply_runs columns): it carries
// exactly what the Acolyte needs to replay the render steps.
//
// Invariant A (ADR-027): the recipe carries vault-refs AS-IS — Input holds
// the operator's `incarnation.spec.input` with string `vault:` references,
// secrets are NOT resolved. essence / RenderedTask / ApplyRequest are never
// stored in the recipe — the Acolyte resolves them in RAM at claim time and
// hands them to the Soul; resolved secrets and the finished render never
// land in PG. StartedByAID is needed for the audit ctx when resolving vault
// at claim (on whose behalf the secret is read).
type Recipe struct {
	// ServiceRef is the git coordinates of the Service repo for loading the
	// artifact at claim time (same type RunSpec carries; reused, not mirrored).
	ServiceRef artifact.ServiceRef `json:"service_ref"`
	// ScenarioName is the scenario name (snake_case), the entry point
	// `scenario/<name>/main.yml`.
	ScenarioName string `json:"scenario_name"`
	// Input is the operator's `incarnation.spec.input` AS-IS: vault-refs as
	// strings, unresolved (invariant A). nil is allowed (scenario without input).
	Input map[string]any `json:"input,omitempty"`
	// StartedByAID is the AID of the run's initiator, used for the audit ctx
	// when resolving vault at claim. NULL for runs without an Archon identity
	// (Soul-initiated / system).
	StartedByAID *string `json:"started_by_aid,omitempty"`
	// DryRun is the Scry flag (ADR-031): the Acolyte will build
	// `ApplyRequest{dry_run:true}` for this task, and the Soul calls
	// `mod.Plan` instead of `mod.Apply` (pure-read, read-safe-capability
	// required). The field is omitempty/false for forward-compat with old
	// recipes — absence in jsonb is equivalent to false (a normal apply).
	// Only set by the check-drift path (Runner.CheckDrift); the normal
	// run/destroy path leaves it untouched.
	DryRun bool `json:"dry_run,omitempty"`
	// FromUpgrade means load the scenario from upgrade/<slug>/ rather than
	// scenario/<slug>/ (ADR-0068): at claim time the Acolyte re-renders the
	// upgrade run the same way the run-goroutine does. omitempty/false for
	// forward-compat — absence in jsonb == the normal scenario/ path. Set by
	// the autorun's found branch (RunSpec.FromUpgrade).
	FromUpgrade bool `json:"from_upgrade,omitempty"`
}

// MarshalRecipe serializes the recipe into the jsonb form of the
// apply_runs.recipe column. A nil recipe → (nil, nil): the old
// Insert(running) path carries no recipe, so SQL NULL is written to the
// column.
func MarshalRecipe(r *Recipe) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("applyrun: marshal recipe: %w", err)
	}
	return b, nil
}

// UnmarshalRecipe restores the recipe from the column's jsonb bytes. An
// empty input (nil / len 0 — SQL NULL for old-path rows) → (nil, nil): a
// missing recipe is not an error at the type level (non-NULL for the claim
// path is an invariant of the claim logic, not of the parser).
func UnmarshalRecipe(b []byte) (*Recipe, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var r Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("applyrun: unmarshal recipe: %w", err)
	}
	return &r, nil
}
