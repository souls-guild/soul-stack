package herald

import "github.com/souls-guild/soul-stack/shared/audit"

// matchIncarnation проверяет опц. селектор Tiding.Incarnation против события.
// nil-селектор → true (без фильтра). Иначе событие проходит, только если в
// его payload есть привязка к инкарнации, равная селектору.
//
// Источник привязки по фактическим payload-формам run-scope-эмиттеров:
//   - incarnation.drift_checked → payload["name"] (incarnation.go/reaper.scry).
//
// Прочие run-события привязки к ОДНОЙ инкарнации в payload НЕ несут: Voyage
// (scenario_run.*/command_run.*/voyage.*) исполняется над МНОЖЕСТВОМ
// инкарнаций (scope), единственного incarnation-поля у его событий нет
// (voyageorch.emitCreated/emitFinalized — есть scope_size/target, не одно имя).
// cadence.* привязан к cadence_id, не к incarnation.
//
// Следствие (документированный trade-off): Tiding с заданным `incarnation`-
// селектором матчит ТОЛЬКО drift_checked-события этой инкарнации; на
// scenario_run/command_run/voyage/cadence-события incarnation-селектор не
// срабатывает (нет поля → не матч). Это консервативно: лучше не уведомить,
// чем уведомить о событии, чья привязка к инкарнации в payload не выражена.
func matchIncarnation(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	v := eventIncarnation(et, payload)
	return v != "" && v == *sel
}

// matchCadence проверяет опц. селектор Tiding.Cadence против события.
// nil-селектор → true. Иначе требует payload-привязку к cadence, равную
// селектору.
//
// Источник привязки по фактическим payload-формам:
//   - cadence.spawned / cadence.skipped_overlap → payload["cadence_id"]
//     (conductor.cadence_spawn).
//
// После BUG-1-фикса (ADR-052 §l amend) cadence-селектор матчит НЕ только
// cadence.*-события: Voyage-терминалы scenario_run.*/command_run.* и
// per-incarnation incarnation.run_completed, спавненные расписанием, несут
// cadence_id в payload прогона. Источник привязки — [eventCadence], который
// извлекает cadence_id из всех этих event_type'ов. Поэтому Tiding с
// cadence-селектором ловит и терминалы расписанных прогонов, а не только
// служебные cadence.spawned/cadence.skipped_overlap. Ручной прогон cadence_id
// не несёт → "" → не матч.
func matchCadence(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	v := eventCadence(et, payload)
	return v != "" && v == *sel
}

// matchVoyage проверяет привязку ephemeral-Tiding к конкретному прогону
// (ADR-052(g)): разовое правило матчит ТОЛЬКО события СВОЕГО Voyage. sel — это
// Tiding.VoyageID (для ephemeral-правил гарантированно непустой, инвариант
// ephemeral⟺voyage_id). Для постоянных правил эта проверка не вызывается (sel nil).
//
// Источник voyage_id события — payload["voyage_id"] (его несут все run-scope-
// эмиттеры: voyageorch.emitFinalized/emitLeg*/emitLeaseLost кладут voyage_id и в
// payload, и в CorrelationID). Fallback на correlation_id (для событий, чей
// payload voyage_id не несёт, но correlation_id = voyage_id — например
// reaper.voyage_reclaim). Совпадения нет → не матч (разовое правило не должно
// сработать на чужой прогон).
func matchVoyage(sel *string, correlationID string, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	if v := payloadStr(payload, "voyage_id"); v != "" {
		return v == *sel
	}
	return correlationID != "" && correlationID == *sel
}

// eventIncarnation извлекает имя инкарнации из payload run-события или "" если
// событие привязки к одной инкарнации не несёт (см. [matchIncarnation]).
func eventIncarnation(et audit.EventType, payload map[string]any) string {
	if et == audit.EventIncarnationDriftChecked {
		return payloadStr(payload, "name")
	}
	return ""
}

// eventCadence извлекает cadence_id из payload cadence-события или "".
//
// incarnation.run_completed (T4b): per-incarnation прогон, заспавненный
// Cadence-расписанием, несёт cadence_id в payload (scenario.Runner кладёт его
// при spec.CadenceID != nil, ADR-052 §k). Нужен task-селектору «алерт на
// таску X» (matchTask матчит только incarnation.run_completed).
//
// Voyage-терминалы scenario_run.*/command_run.* (ADR-052 §l amend): Voyage,
// спавненный расписанием, несёт cadence_id на терминале прогона
// (voyageorch.emitFinalized при run.CadenceID != nil). Поэтому cadence-селектор
// Tiding ловит ОДНО агрегированное уведомление на спавн расписания — а не
// рассыпается на per-incarnation incarnation.run_completed. Ручной Voyage
// cadence_id не несёт → "" → не матч (консервативно).
func eventCadence(et audit.EventType, payload map[string]any) string {
	switch et {
	case audit.EventCadenceSpawned, audit.EventCadenceSkippedOverlap,
		audit.EventIncarnationRunCompleted:
		return payloadStr(payload, "cadence_id")
	case audit.EventScenarioRunCompleted, audit.EventScenarioRunFailed,
		audit.EventScenarioRunPartialFailed,
		audit.EventCommandRunCompleted, audit.EventCommandRunFailed,
		audit.EventCommandRunPartialFailed:
		return payloadStr(payload, "cadence_id")
	default:
		return ""
	}
}

// matchTask проверяет опц. селектор Tiding.Task против события (ADR-052 §l).
// nil-селектор → true (без фильтра). Иначе матчит ТОЛЬКО incarnation.run_completed,
// в payload changed_tasks которого есть запись с register == *sel ИЛИ id == *sel.
//
// changed_tasks — массив map'ов адресов задач (форма changedTasksPayload,
// scenario/run.go): каждый элемент несёт register/id + метаданные/counts.
// Присутствие адреса в changed_tasks = задача изменилась хотя бы на одном хосте
// (ADR-052 §j), поэтому task-селектор самодостаточен — отдельной проверки
// «изменилась» не нужно.
//
// Терпим к обоим представлениям массива: in-process эмиссия даёт
// []map[string]any (tap видит сырой payload эмиттера, не маскированную копию),
// JSON round-trip — []any из map'ов. Пустой адрес (register=="" и id=="") *sel
// не матчит: *sel непуст (validateTiding нормализует ""→nil), равенство ниже это
// отсекает само. Любой иной event_type (нет changed_tasks) → не матч.
func matchTask(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	// Defence-in-depth: пустой селектор не матчит пустой адрес неадресуемой задачи
	// (register=="" && id==""). validateTiding нормализует ""→nil ДО записи, поэтому
	// сюда *sel="" просочиться не должен; страховка отсекает мёртвый случай явно,
	// не полагаясь только на CRUD-нормализацию.
	if *sel == "" {
		return false
	}
	if et != audit.EventIncarnationRunCompleted {
		return false
	}
	for _, m := range changedTasksEntries(payload) {
		if payloadStr(m, "register") == *sel || payloadStr(m, "id") == *sel {
			return true
		}
	}
	return false
}

// changedTasksEntries извлекает записи changed_tasks из payload
// incarnation.run_completed как срез map'ов. Терпим к фактическим типам массива:
//   - []map[string]any — in-process эмиссия (changedTasksPayload, scenario/run.go);
//   - []any из map'ов   — после JSON round-trip (теоретический десериализованный payload).
//
// Отсутствие/иная форма → nil (len 0). Элементы не-map в []any-форме пропускаются.
func changedTasksEntries(payload map[string]any) []map[string]any {
	switch raw := payload["changed_tasks"].(type) {
	case []map[string]any:
		return raw
	case []any:
		out := make([]map[string]any, 0, len(raw))
		for _, entry := range raw {
			if m, ok := entry.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// payloadStr читает строковое поле payload (отсутствие/не-строка → "").
func payloadStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
