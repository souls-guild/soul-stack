package herald

import (
	"strings"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// eventArea returns `<area>` portion of event `<area>.<action>`. For
// non-dotted/empty type returns whole type (in practice all audit-event-types
// are dotted, see naming-rules.md → Audit-events).
func eventArea(et audit.EventType) string {
	s := string(et)
	if i := strings.IndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}

// matchEventType returns true if pattern (Tiding.EventTypes element) covers
// event type et. pattern is either exact `<area>.<action>` or area-glob
// `<area>.*` (semantics per [validateEventType]: only allowed wildcard
// form is `.*` suffix of whole area). Arbitrary wildcard in pattern doesn't
// reach here — filtered at CRUD validation ([ValidateEventTypes]).
func matchEventType(pattern string, et audit.EventType) bool {
	if strings.HasSuffix(pattern, ".*") {
		area := pattern[:len(pattern)-2]
		return eventArea(et) == area
	}
	return pattern == string(et)
}

// isFailureEvent classifies run event as "failure" for only_failures filter
// (ADR-052(c)). Failures are run terminals with non-zero failed outcome:
//   - scenario_run.failed / scenario_run.partial_failed,
//   - command_run.failed / command_run.partial_failed,
//   - scenario_run.lease_lost (run crashed — failover outcome),
//   - voyage.reclaimed (stale lease returned by Reaper — anomaly).
//
// drift_checked is NOT failure by status (drift = divergence, not
// run failure) — filtering for it is done by only_changes. cadence.skipped_overlap
// is not failure (normal skip). started/invoked/created/leg_*/completed are not
// failures. Mapping built from actual event-type emitters
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
		// scenario carries succeeded in nested summary.
		if s, ok := payload["summary"].(map[string]any); ok {
			return payloadInt(s, "succeeded") > 0
		}
		return false

	case audit.EventCommandRunCompleted,
		audit.EventCommandRunPartialFailed:
		// command carries succeeded at root of payload.
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

// driftHostsDrifted extracts drift_summary.hosts_drifted from payload of
// drift_checked event (incarnation.go / reaper.scry). Missing/other form → 0.
func driftHostsDrifted(payload map[string]any) int {
	ds, ok := payload["drift_summary"].(map[string]any)
	if !ok {
		return 0
	}
	return payloadInt(ds, "hosts_drifted")
}

// payloadInt reads integer field from payload map. Tolerant to actual
// numeric types emitters use (int — direct emit) and types after JSON
// round-trip (float64) — dispatcher matches both in-process map from
// emitter and theoretically deserialized payload.
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
