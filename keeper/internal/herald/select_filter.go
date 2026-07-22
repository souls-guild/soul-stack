package herald

import "github.com/souls-guild/soul-stack/shared/audit"

// matchIncarnation checks optional Tiding.Incarnation selector against event.
// nil-selector → true (no filter). Otherwise event passes only if its payload
// carries incarnation binding equal to selector.
//
// Binding source from actual payload forms of run-scope emitters:
//   - incarnation.drift_checked → payload["name"] (incarnation.go/reaper.scry).
//
// Other run events do NOT carry binding to ONE incarnation: Voyage
// (scenario_run.*/command_run.*/voyage.*) executes over MULTIPLE
// incarnations (scope), no single incarnation field in its events
// (voyageorch.emitCreated/emitFinalized have scope_size/target, not one name).
// cadence.* bound to cadence_id, not incarnation.
//
// Consequence (documented trade-off): Tiding with `incarnation` selector
// matches ONLY drift_checked events of that incarnation; on
// scenario_run/command_run/voyage/cadence events incarnation selector doesn't
// fire (no field → no match). Conservative: better not notify than notify
// about event whose incarnation binding isn't expressed in payload.
func matchIncarnation(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	v := eventIncarnation(et, payload)
	return v != "" && v == *sel
}

// matchCadence checks optional Tiding.Cadence selector against event.
// nil-selector → true. Otherwise requires payload binding to cadence equal
// to selector.
//
// Binding source from actual payload forms:
//   - cadence.spawned / cadence.skipped_overlap → payload["cadence_id"]
//     (conductor.cadence_spawn).
//
// After BUG-1 fix (ADR-052 §l amend) cadence selector matches NOT only
// cadence.* events: Voyage terminals scenario_run.*/command_run.* and
// per-incarnation incarnation.run_completed spawned by schedule carry
// cadence_id in run payload. Binding source is [eventCadence], which
// extracts cadence_id from all these event_types. So Tiding with
// cadence selector catches terminal of scheduled runs, not only
// service cadence.spawned/cadence.skipped_overlap. Manual run carries no
// cadence_id → "" → no match.
func matchCadence(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	v := eventCadence(et, payload)
	return v != "" && v == *sel
}

// matchVoyage checks binding of ephemeral-Tiding to concrete run
// (ADR-052(g)): ephemeral rule matches ONLY events of its Voyage. sel is
// Tiding.VoyageID (guaranteed non-empty for ephemeral rules, invariant
// ephemeral⟺voyage_id). For permanent rules this check not called (sel nil).
//
// Event voyage_id source is payload["voyage_id"] (carried by all run-scope
// emitters: voyageorch.emitFinalized/emitLeg*/emitLeaseLost put voyage_id in both
// payload and CorrelationID). Fallback to correlation_id (for events whose
// payload doesn't carry voyage_id but correlation_id = voyage_id — e.g.
// reaper.voyage_reclaim). No match → no match (ephemeral rule shouldn't
// fire on foreign run).
func matchVoyage(sel *string, correlationID string, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	if v := payloadStr(payload, "voyage_id"); v != "" {
		return v == *sel
	}
	return correlationID != "" && correlationID == *sel
}

// eventIncarnation extracts incarnation name from run event payload or ""
// if event doesn't carry binding to one incarnation (see [matchIncarnation]).
func eventIncarnation(et audit.EventType, payload map[string]any) string {
	if et == audit.EventIncarnationDriftChecked {
		return payloadStr(payload, "name")
	}
	return ""
}

// eventCadence extracts cadence_id from cadence event payload or "".
//
// incarnation.run_completed (T4b): per-incarnation run spawned by
// Cadence schedule carries cadence_id in payload (scenario.Runner puts it
// if spec.CadenceID != nil, ADR-052 §k). Needed for task selector "alert on
// task X" (matchTask matches only incarnation.run_completed).
//
// Voyage terminals scenario_run.*/command_run.* (ADR-052 §l amend): Voyage
// spawned by schedule carries cadence_id on run terminal
// (voyageorch.emitFinalized if run.CadenceID != nil). So cadence selector
// of Tiding catches ONE aggregated notification on schedule spawn — not
// dispersed to per-incarnation incarnation.run_completed. Manual Voyage
// carries no cadence_id → "" → no match (conservative).
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

// matchTask checks optional Tiding.Task selector against event (ADR-052 §l).
// nil-selector → true (no filter). Otherwise matches ONLY incarnation.run_completed
// whose payload changed_tasks has entry with register == *sel OR id == *sel.
//
// changed_tasks is array of maps of task addresses (changedTasksPayload form,
// scenario/run.go): each element carries register/id + metadata/counts.
// Address presence in changed_tasks = task changed on at least one host
// (ADR-052 §j), so task selector is self-sufficient — no separate "changed"
// check needed.
//
// Tolerant to both array representations: in-process emission gives
// []map[string]any (tap sees raw emitter payload, not masked copy),
// JSON round-trip gives []any of maps. Empty address (register=="" and id=="")
// doesn't match *sel: *sel non-empty (validateTiding normalizes ""→nil),
// equality below filters it. Any other event_type (no changed_tasks) → no match.
func matchTask(sel *string, et audit.EventType, payload map[string]any) bool {
	if sel == nil {
		return true
	}
	// Defence-in-depth: empty selector doesn't match empty address of unaddressable task
	// (register=="" && id==""). validateTiding normalizes ""→nil before write, so
	// *sel="" shouldn't leak here; guard filters dead case explicitly,
	// not relying only on CRUD normalization.
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

// changedTasksEntries extracts changed_tasks entries from
// incarnation.run_completed payload as slice of maps. Tolerant to actual array types:
//   - []map[string]any — in-process emission (changedTasksPayload, scenario/run.go);
//   - []any of maps     — after JSON round-trip (theoretical deserialized payload).
//
// Missing/other form → nil (len 0). Non-map elements in []any form skipped.
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

// payloadStr reads string field from payload (missing/non-string → "").
func payloadStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
