package soul

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Bulk trait-assign (`POST /v1/souls/traits`, ADR-060) — операторская мутация
// jsonb-колонки `souls.traits` массово по selector ∩ scope. Симметрична bulk
// coven-assign ([BulkAssignCoven]/[BulkReplaceCoven] в crud.go): тот же каркас
// (keyset-чанкинг по PK + коммит на чанк + partial-семантика без отката +
// scope-intersection), отличается только set-выражением (jsonb-операторы вместо
// array_append/remove) и тем, что гейт (b) на trait-ключи НЕ навешивается —
// trait-ключ не является RBAC-измерением scope (в отличие от Coven-метки),
// поэтому least-privilege держится одним гейтом (a): целевые хосты ⊆ coven-scope
// оператора (тот же [BulkScope]). Ослаблять гейт (a) нельзя — без него bulk =
// privilege-escalation.

// BulkAssignTraits применяет mode=merge (set/overwrite переданные ключи,
// остальные сохранить) либо mode=remove (удалить переданные ключи) к хостам под
// selector ∩ scope.
//
//   - merge:  delta — map (key → scalar|list); SET `traits = traits || $delta`,
//     идемпотентный отсев `traits IS DISTINCT FROM (traits || $delta)`.
//   - remove: keys — список имён; SET `traits = traits - $keys`, идемпотентный
//     отсев `traits ?| $keys` (хотя бы один ключ присутствует).
//
// Чанкинг, partial-семантика и scope-intersection (гейт a) — те же, что у
// [BulkAssignCoven]. Валидацию delta/keys (формат ключа, scalar-значение) делает
// caller (handler/MCP) до вызова; здесь — defensive re-check режима.
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

// BulkReplaceTraits заменяет ВЕСЬ traits-map хоста на `traits` целиком (выкидывая
// существующие ключи) для хостов под selector ∩ scope. Пустой/nil map допустим —
// это «очистить все traits» (jsonb `{}`). Идемпотентный отсев — `traits IS
// DISTINCT FROM $new` (jsonb-равенство порядко-независимо, канонизация не нужна).
//
// Гейт (a) scope-intersection обязателен и для replace: coven-scoped оператор не
// затрёт traits на чужих хостах.
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

// bulkTraitChunk выполняет один чанк trait-мутации в собственной транзакции:
// keyset-окно `sid > cursor ORDER BY sid LIMIT chunk` под selector ∩ scope +
// jsonb-set-выражение + идемпотентный отсев. Каркас CTE идентичен
// [bulkUpdateChunk]/[bulkReplaceChunk] (coven), отличается set/idem-выражением
// по mode:
//
//   - merge:   SET traits = traits || $j::jsonb; idem traits IS DISTINCT FROM (traits || $j::jsonb)
//   - replace: SET traits = $j::jsonb;           idem traits IS DISTINCT FROM $j::jsonb
//   - remove:  SET traits = traits - $k::text[]; idem traits ?| $k
//
// Возвращает (changedRows, lastSID, scannedRows, err); условие выхода — по
// размеру keyset-окна (scannedRows), а не по changedRows (чанк, где все ключи
// уже на месте, не должен обрывать итерацию).
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
		// pgx маппит nil-slice в NULL; пустой набор ключей — пустой text[]
		// (`traits ?| ARRAY[]` = false → idem-отсев пропустит все строки).
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

// marshalTraitPayload сериализует trait-map в JSON-байты для jsonb-аргумента
// (паттерн [Insert] — pgx-codec-auto для jsonb сознательно не используется,
// единообразно с прочими jsonb-колонками). nil/пустой map → `{}` (валидный
// пустой jsonb-объект, нужный для replace=«очистить» и merge без ключей).
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
