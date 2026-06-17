package herald

import (
	"strings"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// eventArea возвращает область `<area>` события `<area>.<action>`. Для
// бездотового/пустого типа — весь тип (на практике все audit-event-types
// дотовые, см. naming-rules.md → Audit-events).
func eventArea(et audit.EventType) string {
	s := string(et)
	if i := strings.IndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}

// matchEventType — true, если pattern (элемент Tiding.EventTypes) покрывает
// тип события et. pattern — либо точный `<area>.<action>`, либо area-glob
// `<area>.*` (семантика [validateEventType]: единственная допустимая форма
// wildcard — суффикс `.*` всей области). Произвольный wildcard в pattern не
// попадает сюда — он отсекается на CRUD-валидации ([ValidateEventTypes]).
func matchEventType(pattern string, et audit.EventType) bool {
	if strings.HasSuffix(pattern, ".*") {
		area := pattern[:len(pattern)-2]
		return eventArea(et) == area
	}
	return pattern == string(et)
}

// isFailureEvent классифицирует событие прогона как «провал» для фильтра
// only_failures (ADR-052(c)). Провалом считаются терминалы прогона с
// ненулевым failed-исходом:
//   - scenario_run.failed / scenario_run.partial_failed,
//   - command_run.failed / command_run.partial_failed,
//   - scenario_run.lease_lost (прогон сорвался — failover-исход),
//   - voyage.reclaimed (протухший lease возвращён Reaper-ом — аномалия).
//
// drift_checked провалом по статусу НЕ считается (drift = расхождение, не
// сбой прогона) — для него отбор делает only_changes. cadence.skipped_overlap
// — не провал (штатный skip). started/invoked/created/leg_*/completed — не
// провал. Маппинг построен по фактическим event-types-эмиттерам
// (voyageorch.emitFinalized / emitLeaseLost, reaper.voyage_reclaim).
func isFailureEvent(et audit.EventType) bool {
	switch et {
	case audit.EventScenarioRunFailed,
		audit.EventScenarioRunPartialFailed,
		audit.EventScenarioRunLeaseLost,
		audit.EventCommandRunFailed,
		audit.EventCommandRunPartialFailed,
		audit.EventVoyageReclaimed:
		return true
	default:
		return false
	}
}

// hasChanges классифицирует событие как «несущее изменения» для фильтра
// only_changes (ADR-052(c)). По фактическим payload-формам эмиттеров:
//
//   - incarnation.drift_checked: changed ⇔ drift_summary.hosts_drifted > 0
//     (Scry нашёл расхождение, payload incarnation.go/reaper.scry).
//   - scenario_run.leg_completed: changed ⇔ succeeded > 0 (Leg реально
//     применил часть инкарнаций; payload voyageorch.emitLegCompleted).
//   - scenario_run.completed / command_run.completed / *_partial_failed:
//     changed ⇔ summary.succeeded > 0 (succeeded — у command в корне
//     payload, у scenario во вложенном `summary`; voyageorch.emitFinalized).
//
// Прочие run-scope-события (started/invoked/leg_started/cadence.*/failed без
// успехов/lease_lost/reclaimed) изменений по payload-у НЕ несут → false.
// Решение по семантике документировано: «changes» = «что-то применилось»
// (есть успешный исход), а не «прогон завершился».
//
// Консервативность: при отсутствии ожидаемого поля в payload (форма иная)
// возвращаем false — лучше пропустить уведомление, чем солгать о changes,
// которых не видно. only_changes — отбор «шумных» прогонов, false-negative
// безопаснее false-positive.
func hasChanges(et audit.EventType, payload map[string]any) bool {
	switch et {
	case audit.EventIncarnationDriftChecked:
		return driftHostsDrifted(payload) > 0

	case audit.EventScenarioRunLegCompleted:
		return payloadInt(payload, "succeeded") > 0

	case audit.EventScenarioRunCompleted,
		audit.EventScenarioRunPartialFailed:
		// scenario несёт succeeded во вложенном summary.
		if s, ok := payload["summary"].(map[string]any); ok {
			return payloadInt(s, "succeeded") > 0
		}
		return false

	case audit.EventCommandRunCompleted,
		audit.EventCommandRunPartialFailed:
		// command несёт succeeded в корне payload.
		return payloadInt(payload, "succeeded") > 0

	case audit.EventIncarnationRunCompleted:
		// per-incarnation итог (ADR-052 §k): changed ⇔ есть хоть одна изменившаяся
		// задача (changed_tasks непуст — каждая запись = адрес, изменившийся хотя бы
		// на одном хосте, ADR-052 §j). Нужен для консистентности only_changes с
		// task-селектором (ADR-052 §l): task-селектор сам по себе самодостаточен
		// (присутствие в changed_tasks = изменилась), но если оператор скомбинирует
		// его с only_changes, тот не должен молча отсеять матчевое событие. failed
		// без изменений (ранний/пустой changed_tasks) → false.
		return len(changedTasksEntries(payload)) > 0

	default:
		return false
	}
}

// driftHostsDrifted извлекает drift_summary.hosts_drifted из payload
// drift_checked-события (incarnation.go / reaper.scry). Отсутствие/иная форма
// → 0.
func driftHostsDrifted(payload map[string]any) int {
	ds, ok := payload["drift_summary"].(map[string]any)
	if !ok {
		return 0
	}
	return payloadInt(ds, "hosts_drifted")
}

// payloadInt читает целочисленное поле из payload-map. Терпим к фактическим
// числовым типам, которыми его кладут эмиттеры (int — прямой эмит) и которыми
// оно может прийти после JSON round-trip (float64) — dispatcher матчит как
// in-process map от эмиттера, так и теоретически десериализованный payload.
func payloadInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}
