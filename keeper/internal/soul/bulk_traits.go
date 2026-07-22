package soul

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Bulk trait-assign (`POST /v1/souls/traits`, ADR-060) — an operator mutation of the
// jsonb column `souls.traits` in bulk over selector ∩ scope. Symmetric to bulk
// coven-assign ([BulkAssignCoven]/[BulkReplaceCoven] in crud.go): the same skeleton
// (keyset chunking by PK + commit per chunk + partial semantics without rollback +
// scope-intersection), differs only in the set expression (jsonb operators instead of
// array_append/remove) and in that gate (b) is NOT applied to trait keys —
// a trait key is not an RBAC scope dimension (unlike a Coven label),
// so least-privilege rests on a single gate (a): target hosts ⊆ the operator's
// coven-scope (the same [BulkScope]). Gate (a) must not be relaxed — without it bulk =
// privilege escalation.

// BulkAssignTraits applies mode=merge (set/overwrite the given keys,
// keep the rest) or mode=remove (delete the given keys) to hosts under
// selector ∩ scope.
//
//   - merge:  delta — a map (key → scalar|list); SET `traits = traits || $delta`,
//     idempotent filter `traits IS DISTINCT FROM (traits || $delta)`.
//   - remove: keys — a list of names; SET `traits = traits - $keys`, idempotent
//     filter `traits ?| $keys` (at least one key present).
//
// Chunking, partial semantics, and scope-intersection (gate a) — the same as
// [BulkAssignCoven]. Validation of delta/keys (key format, scalar value) is done by the
// caller (handler/MCP) before the call; here — a defensive re-check of the mode.
func BulkAssignTraits(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, mode TraitMode, delta map[string]any, keys []string) (Report, error) {
	switch mode {
	case TraitMerge:
		if err := ValidateTraitDelta(delta); err != nil {
			return Report{}, err
		}
	case TraitRemove:
		if err := ValidateTraitKeys(keys); err != nil {
			return Report{}, err
		}
	default:
		return Report{}, fmt.Errorf("soul: bulk trait mode %q unsupported (want merge/remove; use BulkReplaceTraits for replace)", mode)
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkTraitChunk(ctx, db, sel, scope, mode, delta, keys, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// BulkReplaceTraits replaces the ENTIRE traits map of a host with `traits` wholesale
// (discarding existing keys) for hosts under selector ∩ scope. An empty/nil map is
// allowed — it means "clear all traits" (jsonb `{}`). The idempotent filter is `traits IS
// DISTINCT FROM $new` (jsonb equality is order-independent, no canonicalization needed).
//
// Gate (a) scope-intersection is mandatory for replace too: a coven-scoped operator must not
// overwrite traits on hosts outside their scope.
func BulkReplaceTraits(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, traits map[string]any) (Report, error) {
	if err := ValidateTraitDelta(traits); err != nil {
		return Report{}, err
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkTraitChunk(ctx, db, sel, scope, TraitReplace, traits, nil, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// bulkTraitChunk executes one chunk of the trait mutation in its own transaction:
// a keyset window `sid > cursor ORDER BY sid LIMIT chunk` under selector ∩ scope +
// the jsonb set expression + an idempotent filter. The CTE skeleton is identical to
// [bulkUpdateChunk]/[bulkReplaceChunk] (coven), differing only in the set/idem expression
// per mode:
//
//   - merge:   SET traits = traits || $j::jsonb; idem traits IS DISTINCT FROM (traits || $j::jsonb)
//   - replace: SET traits = $j::jsonb;           idem traits IS DISTINCT FROM $j::jsonb
//   - remove:  SET traits = traits - $k::text[]; idem traits ?| $k
//
// Returns (changedRows, lastSID, scannedRows, err); the exit condition is based on the
// keyset window size (scannedRows), not changedRows (a chunk where all keys are
// already in place must not break the iteration).
func bulkTraitChunk(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, mode TraitMode, delta map[string]any, keys []string, cursor string) (changed int64, lastSID string, scanned int, err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk traits begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	where, args := buildBulkWhereWithCursor(sel, scope, cursor)

	var setExpr, idemPred string
	switch mode {
	case TraitMerge:
		payload, merr := marshalTraitPayload(delta)
		if merr != nil {
			return 0, "", 0, merr
		}
		pos := len(args) + 1
		args = append(args, payload)
		setExpr = fmt.Sprintf("traits || $%d::jsonb", pos)
		idemPred = fmt.Sprintf("traits IS DISTINCT FROM (traits || $%d::jsonb)", pos)
	case TraitReplace:
		payload, merr := marshalTraitPayload(delta)
		if merr != nil {
			return 0, "", 0, merr
		}
		pos := len(args) + 1
		args = append(args, payload)
		setExpr = fmt.Sprintf("$%d::jsonb", pos)
		idemPred = fmt.Sprintf("traits IS DISTINCT FROM $%d::jsonb", pos)
	case TraitRemove:
		// pgx maps a nil slice to NULL; an empty key set — an empty text[]
		// (`traits ?| ARRAY[]` = false → the idem filter would skip all rows).
		canonical := keys
		if canonical == nil {
			canonical = []string{}
		}
		pos := len(args) + 1
		args = append(args, canonical)
		setExpr = fmt.Sprintf("traits - $%d::text[]", pos)
		idemPred = fmt.Sprintf("traits ?| $%d::text[]", pos)
	default:
		return 0, "", 0, fmt.Errorf("soul: bulkTraitChunk: unsupported mode %q", mode)
	}

	sql := fmt.Sprintf(`
WITH chunk AS (
    SELECT sid FROM souls%s
    ORDER BY sid LIMIT %d
),
upd AS (
    UPDATE souls
    SET traits = %s
    WHERE sid IN (SELECT sid FROM chunk) AND %s
    RETURNING sid
)
SELECT
    (SELECT COUNT(*) FROM chunk),
    (SELECT COUNT(*) FROM upd),
    (SELECT MAX(sid) FROM chunk)
`, where, bulkChunkSize, setExpr, idemPred)

	var (
		scannedN int
		changedN int64
		maxSID   *string
	)
	if err := tx.QueryRow(ctx, sql, args...).Scan(&scannedN, &changedN, &maxSID); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk traits chunk update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk traits chunk commit: %w", err)
	}
	committed = true

	last := ""
	if maxSID != nil {
		last = *maxSID
	}
	return changedN, last, scannedN, nil
}

// marshalTraitPayload serializes a trait map into JSON bytes for the jsonb argument
// (the [Insert] pattern — the pgx auto-codec for jsonb is deliberately not used,
// for consistency with the other jsonb columns). nil/empty map → `{}` (a valid
// empty jsonb object, needed for replace="clear" and merge with no keys).
func marshalTraitPayload(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("soul: marshal trait payload: %w", err)
	}
	return b, nil
}
