package incarnation

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Trait релоцирован per-soul → per-incarnation (ADR-060 amend, R1). Источник
// истины — `incarnation.traits` (operator-set, задаётся в incarnation.spec при
// create). Read-слой остался per-soul (`souls.traits`: soulprint.self.traits /
// where:traits / soul-lint / topology) — он становится PROJECTION TARGET, не
// operator-set-per-soul. Этот файл несёт два моста между источником и target-ом:
//
//   - TraitsFromSpec — operator-set (incarnation.spec.traits) → колонка
//     incarnation.traits (на create-пути);
//   - SyncTraitsToHosts — МАТЕРИАЛИЗОВАННАЯ проекция incarnation.traits в
//     souls.traits хостов-членов (sync-hook на create + bind).

// TraitsFromSpec извлекает operator-set traits из freeform jsonb-spec инкарнации
// (`incarnation.spec.traits`, ADR-060 amend R1). Симметрично [readSpecHosts]
// (тот читает spec["hosts"]): отсутствие ключа / не-map форма → nil (traits не
// заданы), без ошибки (spec freeform). Значение каждого ключа полиморфно
// (scalar | list) — форму валидирует [soul.ValidateTraitDelta], как и per-soul
// bulk-write; невалидный набор → ошибка (caller 422-ит на create-пути ДО insert).
//
// nil-результат на create-пути попадёт в колонку как `{}` (NOT NULL DEFAULT,
// marshalJSONB(nil) → `{}`): «инкарнация без traits».
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

// SyncTraitsToHosts — sync-hook релокации Trait (ADR-060 amend, R1): проецирует
// `incarnation.traits` МАТЕРИАЛИЗОВАННО в `souls.traits` ВСЕХ хостов-членов
// инкарнации `incName`. Член инкарнации = хост, у которого имя инкарнации есть
// в coven[] (ADR-008: incarnation.name — корневая Coven-метка), что выражает
// [soul.BulkSelector.Incarnation].
//
// Точки врезки (sync-hook):
//   - incarnation create (CreateTyped) — после insert строки;
//   - bind хоста через core.soul.registered (keeper-dispatch) — после успешной
//     регистрации, чтобы новопривязанный хост подхватил traits своей инкарнации.
//
// Идемпотентна и повторяема: переиспользует [soul.BulkReplaceTraits] —
// souls.traits хостов-членов ЗАМЕНЯЕТСЯ целиком на incarnation.traits (пустой
// map = очистить). Replace (а не merge) намеренно: incarnation.traits —
// единственный источник истины, проекция выравнивает хосты под него. Это и
// перетирает per-soul bulk-write (POST /v1/souls/traits) в переходный период —
// ожидаемо до relocate per-soul API (следующий слайс, ADR-060 amend).
//
// scope = Unrestricted: это keeper-internal проекция (не operator-инициированный
// bulk), не подчиняется coven-scope оператора — иначе хосты-члены вне scope
// инициатора create не получили бы traits своей инкарнации.
//
// 0 хостов-членов (например на create ДО онбординга) → [soul.BulkReplaceTraits]
// возвращает Matched=0 без ошибки (no-op). [soul.ErrBulkEmptySelector] здесь
// недостижим: selector всегда несёт Incarnation-критерий.
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

// UpdateTraitsResult — итог [UpdateTraits]: снимки old/new ключей для audit-
// payload + полная обновлённая запись incarnation для response. trait-ЗНАЧЕНИЯ
// несёт только [Incarnation.Traits]; OldKeys/NewKeys — лишь имена (секрет-гигиена
// audit-trail, симметрично soul.traits-assign).
type UpdateTraitsResult struct {
	OldKeys     []string
	NewKeys     []string
	Incarnation *Incarnation
}

// UpdateTraits целиком ЗАМЕНЯЕТ operator-set trait-метки инкарнации
// (`incarnation.traits`, ADR-060 amend R1) — зеркало per-soul bulk replace, но на
// уровне источника истины. Тот же транзакционный паттерн, что [UpdateHosts] /
// [Unlock]: одна tx SELECT … FOR UPDATE (сериализация с конкурентным
// Unlock/Upgrade/Destroy/scenario-runner) → UPDATE traits/updated_at → commit.
// Проекция в `souls.traits` хостов-членов делается caller-ом отдельным
// sync-hook-ом ([SyncTraitsToHosts]) ПОСЛЕ commit-а — она вне транзакции
// incarnation (bulk-write по другим строкам, идемпотентна и до-повторяема).
//
// traits валидируется caller-ом ([soul.ValidateTraitDelta]); пустой/nil map —
// «очистить метки» (колонка → `{}`). Возврат [ErrIncarnationNotFound] (404), если
// name не существует. Статус-гейта НЕТ намеренно: traits — operator-set метки,
// не state/spec прогона; replace на любом статусе безопасен (проекция выровняет
// хосты при следующем bind/sync).
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
       last_drift_check_at, last_drift_summary
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

	// nil-map нормализуем к `{}`-проекции: read/response-путь не различает «нет
	// колонки» / «нет меток» (scanIncarnation тоже даёт nil на `{}`).
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

// traitKeys — отсортированный набор ключей trait-map (для audit-payload). nil/
// пустой → пустой slice (устойчивый JSON-вывод).
func traitKeys(traits map[string]any) []string {
	keys := make([]string, 0, len(traits))
	for k := range traits {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
