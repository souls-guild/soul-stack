package cadence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// TargetRecipe is the declarative target of the Cadence recipe, stored as-is
// in the jsonb column `target` (same shape as POST /v1/voyages target —
// ADR-046 §3). Resolved into a unit snapshot at spawn time (Reaper rule
// spawn_due_cadence).
//
// Fields by kind: scenario reads Incarnations/Service/Coven; command reads
// SIDs/Where/Coven. Fields not relevant to the kind are ignored by the resolver.
type TargetRecipe struct {
	// scenario mode:
	Incarnations []string `json:"incarnations,omitempty"`
	Service      string   `json:"service,omitempty"`
	// command mode:
	SIDs  []string `json:"sids,omitempty"`
	Where string   `json:"where,omitempty"`
	// shared incarnation env tag / host coven label:
	Coven []string `json:"coven,omitempty"`
}

// ScenarioResolver resolves a Cadence scenario-target into a snapshot of
// incarnation NAMES (parity handlers.VoyageScenarioResolver). The spawn path
// re-resolves the target just-in-time (the snapshot is fixed at spawn time,
// not at Cadence creation).
type ScenarioResolver interface {
	ResolveIncarnations(ctx context.Context, incarnations []string, service, coven string) ([]string, error)
}

// CommandResolver resolves a Cadence command-target into a snapshot of SIDs
// (parity handlers.VoyageCommandResolver). requireAlive is the alive-presence
// filter.
type CommandResolver interface {
	ResolveSIDs(ctx context.Context, sids, covens []string, where string, requireAlive bool) ([]string, error)
}

// effectiveBatchSize resolves the effective batch size (parity handler
// effectiveBatchSize, ADR-043 amendment §2): batch_percent → ceil(scope*pct/100),
// clamp [1, scope]; otherwise batch_size as-is (nil = the whole run in one Leg).
func effectiveBatchSize(batchSize, batchPercent *int, scope int) *int {
	if batchPercent == nil || scope <= 0 {
		return batchSize
	}
	eff := (scope*(*batchPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// effectiveFailThreshold resolves the fail threshold at spawn-scope (ADR-043
// amendment 2026-06-09, Cadence-recipe S3). Mirrors effectiveBatchSize: percent →
// ceil(scope*pct/100), clamp [1, scope]; otherwise the absolute threshold as-is
// (nil ⇒ no threshold). The resolve happens HERE, not at Cadence create-time: the
// Cadence scope (len(resolved)) is only known at spawn time — unlike Voyage, where
// resolveMaxFailuresPercent resolves percent to absolute already at create (there
// the scope is already resolved). The spawned Voyage receives an already-absolute
// FailThreshold (Voyage has no percent column).
func effectiveFailThreshold(failThreshold, failThresholdPercent *int, scope int) *int {
	if failThresholdPercent == nil || scope <= 0 {
		return failThreshold
	}
	eff := (scope*(*failThresholdPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// batchIndexFor is the 0-based Leg index of the i-th unit (chunk by batch_size).
// window → 0 for all (a flat run, no Legs); barrier with nil/<=0 batch_size →
// a single Leg.
func batchIndexFor(i int, batchSize *int, mode voyage.BatchMode) int {
	if mode == voyage.BatchModeWindow || batchSize == nil || *batchSize <= 0 {
		return 0
	}
	return i / *batchSize
}

// totalBatches is the number of Legs. window → 1 (a single window-wave);
// barrier without batch_size → 1 Leg = the whole run; barrier with batch_size
// → ceil(n/bs).
func totalBatches(n int, batchSize *int, mode voyage.BatchMode) int {
	if n == 0 {
		return 0
	}
	if mode == voyage.BatchModeWindow || batchSize == nil || *batchSize <= 0 {
		return 1
	}
	bs := *batchSize
	return (n + bs - 1) / bs
}

// BuildVoyage assembles a Voyage row + targets-snapshot from the Cadence
// recipe and the resolved unit set (incarnation names for scenario / SIDs for
// command). voyageID is a caller-generated ULID; cadenceID is set as a
// back-link (ADR-046 §2). batch_mode is written only for window (barrier ⇒
// NULL, forward-compat, parity handler.buildVoyageRow).
//
// startedByAID = Cadence's created_by_aid (spawn on behalf of the creator, ADR-046 §7).
func BuildVoyage(c *Cadence, voyageID string, resolved []string) (*voyage.Voyage, []voyage.VoyageTarget) {
	mode := voyage.BatchMode(voyage.BatchModeBarrier)
	if c.BatchMode != nil {
		mode = voyage.BatchMode(*c.BatchMode)
	}
	effBatch := effectiveBatchSize(c.BatchSize, c.BatchPercent, len(resolved))
	effFailThreshold := effectiveFailThreshold(c.FailThreshold, c.FailThresholdPercent, len(resolved))

	targetKind := voyage.TargetKindIncarnation
	if c.Kind == KindCommand {
		targetKind = voyage.TargetKindSID
	}
	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, id := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: targetKind,
			TargetID:   id,
			BatchIndex: batchIndexFor(i, effBatch, mode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	resolvedJSON, _ := json.Marshal(resolved) // []string always marshals

	v := &voyage.Voyage{
		VoyageID:       voyageID,
		Kind:           voyage.Kind(c.Kind),
		ScenarioName:   c.ScenarioName,
		Module:         c.Module,
		Input:          c.Input,
		TargetResolved: resolvedJSON,
		TargetOrigin:   json.RawMessage(c.Target),
		Concurrency:    c.Concurrency,
		TotalBatches:   totalBatches(len(resolved), effBatch, mode),
		StartedByAID:   c.CreatedByAID,
		FailThreshold:  effFailThreshold,
		RequireAlive:   c.RequireAlive,
		CadenceID:      &c.ID,
	}
	if c.OnFailure != nil {
		of := voyage.OnFailure(*c.OnFailure)
		v.OnFailure = &of
	}
	if mode == voyage.BatchModeWindow {
		bm := mode
		v.BatchMode = &bm
		// window: batch_size/batch_percent are unused (window width =
		// concurrency), inter_unit_interval is the per-unit pause.
		v.InterUnitInterval = c.InterUnitInterval
	} else {
		v.BatchSize = effBatch
		v.BatchPercent = c.BatchPercent
		v.InterBatchInterval = c.InterBatchInterval
	}
	return v, targets
}

// ResolveScope resolves the declarative target of the Cadence recipe into a
// unit snapshot (incarnation names for scenario / SIDs for command) via the
// passed-in resolvers. An empty result is not an error (the caller treats it
// as "no targets, nothing to spawn", parity handler voyage_empty_target — but
// on the spawn path it's a soft skip, not a 422).
func ResolveScope(ctx context.Context, c *Cadence, sr ScenarioResolver, cr CommandResolver) ([]string, error) {
	var recipe TargetRecipe
	if len(c.Target) > 0 {
		if err := json.Unmarshal(c.Target, &recipe); err != nil {
			return nil, fmt.Errorf("cadence: parse target recipe: %w", err)
		}
	}
	switch c.Kind {
	case KindScenario:
		coven := ""
		if len(recipe.Coven) > 0 {
			coven = recipe.Coven[0]
		}
		return sr.ResolveIncarnations(ctx, recipe.Incarnations, recipe.Service, coven)
	case KindCommand:
		return cr.ResolveSIDs(ctx, recipe.SIDs, recipe.Coven, recipe.Where, deref(c.RequireAlive))
	default:
		return nil, fmt.Errorf("cadence: invalid kind %q", c.Kind)
	}
}

func deref(b *bool) bool { return b != nil && *b }

// NewVoyageID generates a ULID for the spawned Voyage (parity
// handler.buildVoyageRow). A var (not a wrapper function) so it can be
// swapped in spawn-rule unit tests.
var NewVoyageID = audit.NewULID
