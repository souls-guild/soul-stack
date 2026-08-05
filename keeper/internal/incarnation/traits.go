package incarnation

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Operator-set trait labels of an incarnation (`incarnation.traits`, ADR-060).
// They stay HERE: nothing projects them onto member hosts (ADR-080 removed the
// materialized `SyncTraitsToHosts` hook). A host reaches them by inheritance at
// read time — its effective traits are its own `souls.traits` unioned with those
// of every incarnation it belongs to (soul.UnionTraits) — which is what lets an
// incarnation label cover hosts that join later without overwriting a label
// attached directly to a host.

// ValidateCreateTraits checks the operator-set traits of a create request and
// returns them in the form the `incarnation.traits` column takes (ADR-060 amend
// R1). Each value is polymorphic (scalar | list); the form is validated by
// [soul.ValidateTraitDelta], the same check the per-soul bulk-write runs, and an
// invalid set is an error the caller answers with 422 BEFORE the insert.
//
// It takes the request's traits DIRECTLY. Until NIM-408 they arrived here inside
// a freeform `spec` map that the create path assembled purely to take them back
// out of it again — and the column has been the source of truth since migration
// 088, so the detour only existed because `spec` was where the request used to be
// persisted. `spec` is gone; the request is read where it arrives.
//
// nil / empty → nil, which the column stores as `{}` (NOT NULL DEFAULT):
// "an incarnation without traits".
func ValidateCreateTraits(traits map[string]any) (map[string]any, error) {
	if len(traits) == 0 {
		return nil, nil
	}
	if err := soul.ValidateTraitDelta(traits); err != nil {
		return nil, fmt.Errorf("incarnation: invalid traits: %w", err)
	}
	return traits, nil
}

// UpdateTraitsResult — result of [UpdateTraits]: snapshots of old/new keys for audit
// payload + full updated incarnation record for response. trait-VALUES
// carried only by [Incarnation.Traits]; OldKeys/NewKeys — names only (secret hygiene
// audit-trail, symmetric with soul.traits-assign).
type UpdateTraitsResult struct {
	OldKeys     []string
	NewKeys     []string
	Incarnation *Incarnation
}

// UpdateTraits entirely REPLACES the operator-set trait labels of an incarnation
// (`incarnation.traits`, ADR-060). Same transactional pattern as [UpdateHosts] /
// [Unlock]: single tx SELECT … FOR UPDATE (serialization with concurrent
// Unlock/Upgrade/Destroy/scenario-runner) → UPDATE traits/updated_at → commit.
//
// The write ends here — no member host is touched (ADR-080). Hosts see the new
// set on their next read through inheritance, so a removed key stops granting at
// once instead of lingering until some projection catches up.
//
// traits validated by caller ([soul.ValidateTraitDelta]); empty/nil map —
// "clear labels" (column → `{}`). Returns [ErrIncarnationNotFound] (404) if
// name doesn't exist. Status-gate intentionally absent: traits are operator-set
// labels, not run state/spec; a replace is safe at any status.
func UpdateTraits(ctx context.Context, pool TxBeginner, name string, traits map[string]any) (*UpdateTraitsResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin update-traits tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT name, service, service_version, state_schema_version,
       state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits,
       last_drift_check_at, last_drift_summary, created_scenario,
       applying_apply_id
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	inc, err := scanIncarnation(tx.QueryRow(ctx, selectForUpdateSQL, name))
	if err != nil {
		return nil, err
	}

	oldKeys := traitKeys(inc.Traits)

	traitsBytes, err := marshalJSONB(traits)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal traits: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET traits     = $2,
    updated_at = NOW()
WHERE name = $1
RETURNING updated_at
`
	if err := tx.QueryRow(ctx, updateSQL, name, traitsBytes).Scan(&inc.UpdatedAt); err != nil {
		return nil, fmt.Errorf("incarnation: update traits: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit update-traits tx: %w", err)
	}

	// nil-map normalize to `{}` projection: read/response path doesn't distinguish "no
	// column" / "no labels" (scanIncarnation also gives nil on `{}`).
	if traits == nil {
		traits = map[string]any{}
	}
	inc.Traits = traits
	return &UpdateTraitsResult{
		OldKeys:     oldKeys,
		NewKeys:     traitKeys(traits),
		Incarnation: inc,
	}, nil
}

// traitKeys — sorted set of trait-map keys (for audit-payload). nil/
// empty → empty slice (stable JSON output).
func traitKeys(traits map[string]any) []string {
	keys := make([]string, 0, len(traits))
	for k := range traits {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
