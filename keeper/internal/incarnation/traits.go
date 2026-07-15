package incarnation

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Trait relocated per-soul → per-incarnation (ADR-060 amend, R1). Source of
// truth is `incarnation.traits` (operator-set, given in incarnation.spec on
// create). The read layer stays per-soul (`souls.traits`: soulprint.self.traits /
// where:traits / soul-lint / topology) — it becomes a PROJECTION TARGET, not
// operator-set-per-soul. This file carries two bridges between the source and
// the target:
//
//   - TraitsFromSpec — operator-set (incarnation.spec.traits) → the
//     incarnation.traits column (on the create path);
//   - SyncTraitsToHosts — MATERIALIZED projection of incarnation.traits into
//     souls.traits of member hosts (sync-hook on create + bind).

// TraitsFromSpec extracts operator-set traits from freeform jsonb spec of incarnation
// (`incarnation.spec.traits`, ADR-060 amend R1). Symmetric with [readSpecHosts]
// (which reads spec["hosts"]): missing key / non-map form → nil (traits not
// set), no error (spec freeform). Value of each key polymorphic
// (scalar | list) — form validated by [soul.ValidateTraitDelta], same as per-soul
// bulk-write; invalid set → error (caller 422s on create path BEFORE insert).
//
// nil result on create path goes to column as `{}` (NOT NULL DEFAULT,
// marshalJSONB(nil) → `{}`): "incarnation without traits".
func TraitsFromSpec(spec map[string]any) (map[string]any, error) {
	if spec == nil {
		return nil, nil
	}
	raw, ok := spec["traits"]
	if !ok {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("incarnation: spec.traits must be an object (key → scalar|list), got %T", raw)
	}
	if len(m) == 0 {
		return nil, nil
	}
	if err := soul.ValidateTraitDelta(m); err != nil {
		return nil, fmt.Errorf("incarnation: invalid spec.traits: %w", err)
	}
	return m, nil
}

// SyncTraitsToHosts — sync-hook of Trait relocation (ADR-060 amend, R1): projects
// `incarnation.traits` MATERIALIZED into `souls.traits` of ALL member hosts
// of incarnation `incName`. Member of incarnation = host whose incarnation name exists
// in coven[] (ADR-008: incarnation.name is root Coven label), expressed by
// [soul.BulkSelector.Incarnation].
//
// Hookpoints (sync-hook):
//   - incarnation create (CreateTyped) — after row insert;
//   - host bind via core.soul.registered (keeper-dispatch) — after successful
//     registration so newly bound host picks up traits of its incarnation.
//
// Idempotent and re-runnable: reuses [soul.BulkReplaceTraits] —
// souls.traits of member hosts REPLACED entirely with incarnation.traits (empty
// map = clear). Replace (not merge) intentional: incarnation.traits is
// sole source of truth, projection aligns hosts to it. This also
// overwrites per-soul bulk-write (POST /v1/souls/traits) in transition period —
// expected until per-soul API relocate (next slice, ADR-060 amend).
//
// scope = Unrestricted: this is keeper-internal projection (not operator-initiated
// bulk), not subject to operator's coven scope — otherwise member hosts outside
// create initiator's scope wouldn't get traits of their incarnation.
//
// 0 member hosts (e.g., create before onboarding) → [soul.BulkReplaceTraits]
// returns Matched=0 without error (no-op). [soul.ErrBulkEmptySelector] unreachable
// here: selector always carries Incarnation criterion.
func SyncTraitsToHosts(ctx context.Context, db soul.BulkPool, incName string, traits map[string]any) error {
	if !ValidName(incName) {
		return fmt.Errorf("incarnation: sync traits: invalid name %q", incName)
	}
	sel := soul.BulkSelector{Incarnation: incName}
	scope := soul.BulkScope{Unrestricted: true}
	if _, err := soul.BulkReplaceTraits(ctx, db, sel, scope, traits); err != nil {
		return fmt.Errorf("incarnation: sync traits → souls of %q: %w", incName, err)
	}
	return nil
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

// UpdateTraits entirely REPLACES operator-set trait labels of incarnation
// (`incarnation.traits`, ADR-060 amend R1) — mirror of per-soul bulk replace, but at
// source-of-truth level. Same transactional pattern as [UpdateHosts] /
// [Unlock]: single tx SELECT … FOR UPDATE (serialization with concurrent
// Unlock/Upgrade/Destroy/scenario-runner) → UPDATE traits/updated_at → commit.
// Projection to `souls.traits` of member hosts done by caller with separate
// sync-hook ([SyncTraitsToHosts]) AFTER commit — outside incarnation transaction
// (bulk-write on other rows, idempotent and re-runnable).
//
// traits validated by caller ([soul.ValidateTraitDelta]); empty/nil map —
// "clear labels" (column → `{}`). Returns [ErrIncarnationNotFound] (404) if
// name doesn't exist. Status-gate intentionally absent: traits are operator-set labels,
// not run state/spec; replace safe at any status (projection aligns
// hosts on next bind/sync).
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
       spec, state, status, status_details, created_by_aid,
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
