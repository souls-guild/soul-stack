package handlers

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// secret-схема read-path-маскинга ([ADR-010] §7.4, декларативный слой 1).
// incarnation_view маскирует spec/state/history через [audit.MaskSecretsWithSchema]
// по путям, объявленным secret:true в схеме сервиса:
//
//   - state — из flat state_schema манифеста (`properties.<field>.secret: true`,
//     рекурсивно через properties/items, и через additionalProperties-ВЛОЖЕННЫЕ
//     properties); путь — `<field>`, элемент массива — `<field>[].<...>`.
//     ★ secret НА САМОМ additionalProperties-узле (значение произвольного ключа map)
//     schema-слоем НЕ покрыт (TODO отдельным слайсом) — деградирует к vault+regex
//     (см. ограничение в collectStateSchemaSecrets);
//   - spec — из input_schema scenario `create` (spec несёт операторский input под
//     ключом `input`); путь — `input.<name>` (config.InputSchema.Secret).
//
// Сборка best-effort: материализация снапшота сервиса (git) — НЕ на каждый
// read-роут, а только в [IncarnationHandler.GetTyped]/History (single-incarnation
// детальный вид). Ошибка загрузки/парса → nil-схема → деградация к
// [audit.MaskSecrets] (vault+regex), GET не падает (наблюдаемость, не контракт).

// secretSchemaForIncarnation материализует снапшот сервиса incarnation и строит
// объединённую [audit.SecretPathSet] для state (state_schema) + spec
// (create-scenario input_schema). nil → схема недоступна (loader/services nil,
// ошибка загрузки) — caller деградирует к MaskSecrets.
//
// ctx прокидывается из read-handler-а (отмена/таймаут материализации снапшота).
// Возврат — интерфейс [audit.SecretSchema]: при пустом наборе возвращается
// ИМЕННО nil-интерфейс (а не SecretPathSet(nil)), чтобы caller-ова проверка
// `schema == nil` отличала «схемы нет» от «схема пуста» — иначе непустой
// интерфейс-обёртка над nil-map включила бы schema-слой вхолостую.
func (h *IncarnationHandler) secretSchemaForIncarnation(ctx context.Context, inc *incarnation.Incarnation) audit.SecretSchema {
	if h.loader == nil || h.services == nil || inc == nil {
		return nil
	}
	ref, ok := h.services.Resolve(inc.Service)
	if !ok {
		return nil
	}
	// Зафиксировать ref на версии incarnation (read-path сверяет тот снапшот, по
	// которому incarnation создана/мигрирована).
	if inc.ServiceVersion != "" {
		ref.Ref = inc.ServiceVersion
	}
	art, err := h.loader.Load(ctx, ref)
	if err != nil || art == nil {
		return nil
	}

	set := audit.SecretPathSet{}
	if art.Manifest != nil {
		collectStateSchemaSecrets(art.Manifest.StateSchema, "", set)
	}
	collectCreateInputSecrets(h.loader, art, set)
	if len(set) == 0 {
		return nil
	}
	return set
}

// collectStateSchemaSecrets рекурсивно обходит flat JSON-schema state_schema и
// помечает в set dot/idx-пути полей с `secret: true`. Структура:
//   - properties: map<field, schema> → рекурсия с path = join(path, field);
//   - items: schema → рекурсия с path = path+"[]" (элемент массива);
//   - additionalProperties: schema → рекурсия с path = path (БЕЗ `.*`-сегмента).
//
// ★ Ограничение read-path schema-слоя для secret-leaf под additionalProperties
// (TODO отдельным слайсом): когда `secret: true` стоит на САМОМ ap-узле (значение
// любого ключа map секрет), schema-слой его НЕ покрывает. [audit.SecretPathSet.
// IsSecret] сверяет запрашиваемый путь как есть или через normalizeIdx (конкретные
// индексы → `[]`) — но НИКОГДА не подставляет `.*`-сегмент; запись вида `path.*` в
// наборе ни с одним реальным запросом не совпала бы (мёртвая). Поэтому ap-secret-leaf
// помечать бессмысленно — он деградирует к vault+regex-слою маскинга (MaskSecrets).
// Рекурсию ВЕДЁМ (внутри ap могут быть вложенные `properties` с конкретными именами —
// их пути schema-слой покрывает), а сам secret НА ap-узле пропускаем.
//
// ★ Сопутствующий gap (ВЛОЖЕННЫЙ secret под ap по ДИНАМИЧЕСКОМУ ключу, nit
// seal-review, НЕ чиним — фикс = матчинг dynamic-key в IsSecret, отдельный слайс):
// рекурсия в ap НЕ добавляет сегмента под произвольный ключ, поэтому
// `users:{additionalProperties:{properties:{password:{secret}}}}` собирает путь
// `users.password`. Реальный payload (incarnation.state) несёт КОНКРЕТНЫЙ ключ map —
// `{users:{alice:{password:…}}}` → maskMapLayered ведёт путь ячейки `users.alice.
// password`. [audit.SecretPathSet.IsSecret] сверяет `users.alice.password` И
// normalizeIdx(того же) — а normalizeIdx обобщает ТОЛЬКО индексы среза (`[N]`→`[]`),
// map-ключ `alice` не трогает → ни одна форма не совпадает с собранным `users.
// password`. Schema-слой такой secret НЕ маскирует; он деградирует к vault+regex
// (ключ `password` ловится sensitive-by-name regex-last-resort'ом — alarm-fallback,
// не schema). Регрессию текущего поведения фиксирует тест
// TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret_DynamicKeyGap.
func collectStateSchemaSecrets(schema map[string]any, path string, set audit.SecretPathSet) {
	if schema == nil {
		return
	}
	if isSecretNode(schema) && path != "" {
		set[path] = true
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for field, sub := range props {
			if subm, ok := sub.(map[string]any); ok {
				collectStateSchemaSecrets(subm, joinSchemaPath(path, field), set)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		collectStateSchemaSecrets(items, path+"[]", set)
	}
	if ap, ok := schema["additionalProperties"].(map[string]any); ok {
		// secret НА САМОМ ap-узле не помечаем (см. ★-ограничение в doc-комментарии:
		// путь `map_field` пометил бы ВСЮ map как secret — over-mask на read-path; а
		// `map_field.*` IsSecret не запрашивает → запись мёртвая. Деградация к
		// vault+regex намеренна). Рекурсию ведём, но БЕЗ secret-флага самого ap-узла:
		// вложенные конкретные `properties` внутри ap schema-слой покрывает по точному
		// пути, а secret НА ap-узле гасим, чтобы isSecretNode не пометил path.
		recurse := ap
		if isSecretNode(ap) {
			recurse = mapWithoutSecret(ap)
		}
		collectStateSchemaSecrets(recurse, path, set)
	}
}

// isSecretNode — JSON-schema-узел несёт `secret: true`.
func isSecretNode(schema map[string]any) bool {
	b, _ := schema["secret"].(bool)
	return b
}

// mapWithoutSecret — поверхностная копия schema-узла без ключа `secret`, чтобы
// рекурсия по additionalProperties не пометила сам ap-узел как secret-leaf (его
// path = имя map, пометка дала бы over-mask всей map). Вложенные `properties`/
// `items` копии не трогаются — рекурсия по ним идёт по своим точным путям.
func mapWithoutSecret(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		if k == "secret" {
			continue
		}
		out[k] = v
	}
	return out
}

// collectCreateInputSecrets читает scenario `create`/main.yml снапшота, парсит
// его input-схему (config.InputSchemaMap) и помечает `input.<name>` для
// secret:true-параметров (spec несёт операторский input под ключом `input`).
// Best-effort: нет create-scenario / парс упал → ничего не добавляет.
func collectCreateInputSecrets(loader ServiceSnapshotLoader, art *artifact.ServiceArtifact, set audit.SecretPathSet) {
	data, err := loader.ReadFile(art, "scenario/create/main.yml")
	if err != nil || len(data) == 0 {
		return
	}
	scn, _, sdiags, perr := artifact.LoadScenarioManifestResolved(art, "scenario/create/main.yml", data)
	if perr != nil || scn == nil {
		return
	}
	// fail-closed ТОЛЬКО на covenant-merge-ошибке (симметрично scenario.run/
	// validate_input): при сбое слияния covenant смерженный scn.Input ЧАСТИЧЕН
	// (covenant-поля могли не влиться) — строить secret-маскировку по нему нельзя,
	// иначе secret-поле из covenant молча не замаскируется. Прочие schema-ошибки
	// create-сценария (например `tasks is required` на неполной фикстуре) input НЕ
	// обрезают — best-effort secret-схема по тому, что распарсилось (GET — не
	// контракт-валидатор, см. doc-комментарий пакета). Деградация к vault+regex
	// (caller получит nil-вклад в set) остаётся только для covenant-частичности.
	if hasCovenantMergeError(sdiags) {
		return
	}
	for name, s := range scn.Input {
		if s != nil && s.Secret {
			set["input."+name] = true
		}
	}
}

// covenantMergeErrorCodes — коды диагностик config.ResolveScenarioCovenant/
// MergeCovenant, означающие, что слияние covenant НЕ состоялось → смерженный input
// частичен (covenant-поля не влились). ТОЛЬКО на них secret-схема fail-close-ится
// (covenant-secret-поле иначе молча утекло бы немаскированным). Прочие schema-ошибки
// сценария input не обрезают. Источник кодов — shared/config/covenant_resolve.go и
// covenant.go (держать синхронно: новый covenant-merge-код добавлять и сюда).
var covenantMergeErrorCodes = map[string]bool{
	"covenant_extends_invalid":          true,
	"covenant_extends_target_not_found": true,
	"state_changes_form_mismatch":       true,
	"section_key_conflict":              true,
	"covenant_merge_failed":             true,
	"covenant_unexpected_key":           true,
}

// hasCovenantMergeError — среди diags есть error-уровня covenant-merge-код (см.
// covenantMergeErrorCodes). True → смерженный input частичен, secret-схему по нему
// строить нельзя.
func hasCovenantMergeError(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if d.Level == diag.LevelError && covenantMergeErrorCodes[d.Code] {
			return true
		}
	}
	return false
}

// joinSchemaPath — конкатенация dot-пути state_schema (БИТ-В-БИТ как audit.joinPath
// / render.joinKey: пустой path → field без ведущей точки).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
