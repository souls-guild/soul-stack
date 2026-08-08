package incarnation

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Operator-set trait labels of an incarnation (`incarnation.traits`, ADR-060).
// They describe the INCARNATION and stay there: nothing projects them onto
// member hosts, neither materialized nor unioned at read time (NIM-281). A
// host's traits are the ones an operator attached to that host.
//
// A trait condition therefore never reaches a host through its membership. To
// address an incarnation's members, ask the membership question —
// `incarnation=<name>`, answered from `incarnation_membership`.

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
// The write ends here — no member host is touched, and none ever will be: these
// labels sit on the incarnation and reach nothing else (NIM-281). A rule that
// wants a HOST to carry the pair must be pointed at the host's own traits, which
// only [soul.AssignTraits] writes.
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
       created_scenario,
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

	// `RETURNING traits` and not the bytes we just sent: [Incarnation.TraitsRaw]
	// is defined as the column exactly as POSTGRES serializes it, and jsonb
	// re-canonicalizes on the way in (`1e6` reads back `1000000`). Assigning
	// traitsBytes here would leave the struct holding Go's spelling under a field
	// documented to hold Postgres', which is the NIM-521 divergence reintroduced
	// one layer up. `inc` was scanned BEFORE the update, so leaving TraitsRaw
	// untouched is not an option either — it would keep the OLD labels beside the
	// new map.
	const updateSQL = `
UPDATE incarnation
SET traits     = $2,
    updated_at = NOW()
WHERE name = $1
RETURNING updated_at, traits
`
	if err := tx.QueryRow(ctx, updateSQL, name, traitsBytes).Scan(&inc.UpdatedAt, &inc.TraitsRaw); err != nil {
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
