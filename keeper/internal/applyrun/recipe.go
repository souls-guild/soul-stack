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
// secrets are NOT resolved. Service vars / RenderedTask / ApplyRequest are never
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
	// FromUpgrade means load the scenario from upgrade/<slug>/ rather than
	// scenario/<slug>/ (ADR-0068): at claim time the Acolyte re-renders the
	// upgrade run the same way the run-goroutine does. omitempty/false for
	// forward-compat — absence in jsonb == the normal scenario/ path. Set by
	// the autorun's found branch (RunSpec.FromUpgrade).
	FromUpgrade bool `json:"from_upgrade,omitempty"`
	// Step is set on a row of a linear run's step (NIM-961): the claim renders
	// that step (the row's passage) rather than the whole scenario. nil — the
	// row of a staged run or of the Oracle path, rendered in one pass as before.
	Step *RecipeStep `json:"step,omitempty"`
}

// RecipeStep is what the run goroutine had decided when it wrote a step's rows,
// and the claim-time render must reproduce rather than decide again (ADR-027
// amend (p)). SIDs and register names only — nothing rendered, so invariant A
// holds.
type RecipeStep struct {
	// Roster — the SIDs the run renders this step against. The run re-resolves
	// its roster only at a refresh boundary, so a host that joined the
	// incarnation since is not in it, and a `run_once:` or `soulprint.hosts`
	// rendered over the live roster would disagree with the run's own render.
	Roster []string `json:"roster"`
	// Dropped — the SIDs the run dropped under `on_failure: host` before this step
	// (NIM-948). Not in Roster — nothing targets them — but still in
	// `soulprint.hosts`, flagged `dropped`, so the claim renders the list the run did.
	Dropped []string `json:"dropped,omitempty"`
	// HeldTargets — each default block's hosts, chosen at the block's first
	// step and held for its later ones (render.HeldTargets), by plan position.
	HeldTargets map[string][]string `json:"held_targets,omitempty"`
	// RegisterNames — plan index → register name of the plan's tasks, as the
	// run read the earlier steps' registers: their rows carry the plan index
	// alone.
	RegisterNames map[int]string `json:"register_names,omitempty"`
	// Tasks — plan indices of the tasks the run decided to send this host at
	// this step. A claim whose own render sends anything else refuses the host:
	// the run's roster, a fact or a destiny on a branch moved between the two
	// renders, and the run did not decide what the claim would send.
	Tasks []int `json:"tasks"`
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
