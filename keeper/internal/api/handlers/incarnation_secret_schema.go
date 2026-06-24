package handlers

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// secret-схема read-path-маскинга ([ADR-010] §7.4, декларативный слой 1).
// incarnation_view маскирует spec/state/history через [audit.MaskSecretsWithSchema]
// по путям, объявленным secret:true в схеме сервиса:
//
//   - state — из flat state_schema манифеста (`properties.<field>.secret: true`,
//     рекурсивно через properties/items/additionalProperties); путь —
//     `<field>`, элемент массива — `<field>[].<...>`;
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
//   - additionalProperties: schema → рекурсия с path = path+".*" пропущена
//     (map с произвольными ключами; конкретный путь неизвестен — secret на
//     additionalProperties помечает «значение любого ключа секрет», path = path).
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
		// map с произвольными ключами: secret на самом ap-узле → значение любого
		// ключа секрет. Конкретный ключ неизвестен — обобщённый сегмент `.*`
		// сверяется audit.SecretPathSet.IsSecret через normalizeIdx-симметрию не
		// покрыт; помечаем путь самого map как «дочерние значения секрет» отдельной
		// записью с суффиксом `.*` — read-path-маскинг по точному ключу его не
		// поймает, поэтому рекурсию ведём, а сам ap-secret эскалируем на родителя.
		if isSecretNode(ap) && path != "" {
			set[path+".*"] = true
		}
		collectStateSchemaSecrets(ap, path, set)
	}
}

// isSecretNode — JSON-schema-узел несёт `secret: true`.
func isSecretNode(schema map[string]any) bool {
	b, _ := schema["secret"].(bool)
	return b
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
	scn, _, _, perr := config.LoadScenarioManifestFromBytes("scenario/create/main.yml", data, config.ValidateOptions{})
	if perr != nil || scn == nil {
		return
	}
	for name, s := range scn.Input {
		if s != nil && s.Secret {
			set["input."+name] = true
		}
	}
}

// joinSchemaPath — конкатенация dot-пути state_schema (БИТ-В-БИТ как audit.joinPath
// / render.joinKey: пустой path → field без ведущей точки).
func joinSchemaPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
