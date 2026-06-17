package herald

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// scenarioTerminal — payload Voyage-терминала kind=scenario как его кладёт
// voyageorch.emitFinalized. extra (cadence_id) добавляется при run.CadenceID != nil.
func scenarioTerminal(extra map[string]any) map[string]any {
	p := map[string]any{
		"voyage_id":     "vy_1",
		"kind":          "scenario",
		"total_batches": 1,
		"summary":       map[string]any{"total": 3, "succeeded": 3, "failed": 0, "cancelled": 0},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// commandTerminal — payload Voyage-терминала kind=command (emitFinalized).
func commandTerminal(extra map[string]any) map[string]any {
	p := map[string]any{
		"voyage_id": "vy_1",
		"kind":      "command",
		"total":     5,
		"succeeded": 5,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// TestDispatch_CadenceNotifyDelivery — ГЛАВНЫЙ guard (ADR-052 §l amend, Вариант б):
// именно его отсутствие пропустило QA-blocker. cadence-notify правило
// (event_types=[scenario_run.failed], cadence=<ULID>, created_from_cadence_id=<ULID>)
// против Voyage-терминала scenario_run.failed с payload cadence_id=<ULID> должно
// дойти до доставки — полный dispatcher matchTiding-путь, а не только InsertTiding.
func TestDispatch_CadenceNotifyDelivery(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	failedTerminal := ev(audit.EventScenarioRunFailed, scenarioTerminal(map[string]any{
		"cadence_id": cadenceULID,
		"error_code": "no_match",
		"summary":    map[string]any{"total": 3, "succeeded": 0, "failed": 3, "cancelled": 0},
	}))

	// Правило ровно как его создаёт cadence.notify (created_from_cadence_id —
	// маркер происхождения, на матч не влияет, но фиксируем боевую форму правила).
	cadenceNotifyRule := &Tiding{
		Name:                 "cadence-notify",
		Herald:               "ops",
		EventTypes:           []string{"scenario_run.failed"},
		Cadence:              strPtr(cadenceULID),
		CreatedFromCadenceID: strPtr(cadenceULID),
		Enabled:              true,
	}

	jobs := dispatchOne(t, cadenceNotifyRule, failedTerminal)
	if len(jobs) != 1 {
		t.Fatalf("cadence-notify правило должно доставить ровно 1 job на Voyage-терминал "+
			"scenario_run.failed с cadence_id — получил %d (это QA-blocker: правило не матчило)", len(jobs))
	}
}

// TestDispatch_CadenceNotify_AllTerminals — completed/failed/partial_failed
// Voyage-терминалы scenario_run.* с cadence_id матчатся cadence-правилом по
// соответствующему event_type.
func TestDispatch_CadenceNotify_AllTerminals(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	cases := []struct {
		name string
		et   audit.EventType
	}{
		{"scenario completed", audit.EventScenarioRunCompleted},
		{"scenario failed", audit.EventScenarioRunFailed},
		{"scenario partial_failed", audit.EventScenarioRunPartialFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			event := ev(c.et, scenarioTerminal(map[string]any{"cadence_id": cadenceULID}))
			rule := &Tiding{
				Name: "cadence-notify", Herald: "ops",
				EventTypes: []string{string(c.et)},
				Cadence:    strPtr(cadenceULID),
				Enabled:    true,
			}
			if jobs := dispatchOne(t, rule, event); len(jobs) != 1 {
				t.Fatalf("cadence-правило на %s должно доставить 1 job, получил %d", c.et, len(jobs))
			}
		})
	}
}

// TestDispatch_CadenceNotify_PartialGlob — cadence-правило on:[partial] через
// area-glob scenario_run.* матчит scenario_run.partial_failed с cadence_id.
func TestDispatch_CadenceNotify_PartialGlob(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	partial := ev(audit.EventScenarioRunPartialFailed, scenarioTerminal(map[string]any{
		"cadence_id": cadenceULID,
		"on_failure": "continue",
		"summary":    map[string]any{"total": 3, "succeeded": 2, "failed": 1, "cancelled": 0},
	}))
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"scenario_run.*"},
		Cadence:    strPtr(cadenceULID),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, partial); len(jobs) != 1 {
		t.Fatalf("cadence-правило scenario_run.* должно доставить 1 job на partial_failed с cadence_id, получил %d", len(jobs))
	}
}

// TestDispatch_CadenceNotify_CommandTerminal — command-Voyage (cadence может
// спавнить command-прогон) тоже несёт cadence_id на терминале и матчится.
func TestDispatch_CadenceNotify_CommandTerminal(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	completed := ev(audit.EventCommandRunCompleted, commandTerminal(map[string]any{"cadence_id": cadenceULID}))
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"command_run.*"},
		Cadence:    strPtr(cadenceULID),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, completed); len(jobs) != 1 {
		t.Fatalf("cadence-правило command_run.* должно доставить 1 job на command-терминал с cadence_id, получил %d", len(jobs))
	}
}

// TestDispatch_CadenceNotify_ManualVoyageNotMatched — ручной Voyage (terminal
// без cadence_id) cadence-правилом НЕ матчится (консервативно).
func TestDispatch_CadenceNotify_ManualVoyageNotMatched(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	manual := ev(audit.EventScenarioRunFailed, scenarioTerminal(nil)) // без cadence_id
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"scenario_run.failed"},
		Cadence:    strPtr(cadenceULID),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, manual); len(jobs) != 0 {
		t.Fatalf("cadence-правило не должно матчить ручной Voyage-терминал без cadence_id, получил %d job", len(jobs))
	}
}

// TestDispatch_CadenceNotify_MismatchedID — другой cadence_id на терминале → не матч.
func TestDispatch_CadenceNotify_MismatchedID(t *testing.T) {
	other := ev(audit.EventScenarioRunFailed, scenarioTerminal(map[string]any{"cadence_id": "cd_other"}))
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"scenario_run.failed"},
		Cadence:    strPtr("cd_mine"),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, other); len(jobs) != 0 {
		t.Fatalf("cadence-правило не должно матчить терминал с чужим cadence_id, получил %d job", len(jobs))
	}
}

// TestEventCadence_VoyageTerminals — eventCadence извлекает cadence_id из всех
// шести Voyage-терминалов И по-прежнему из cadence.*/incarnation.run_completed
// (не сломан добавлением scenario_run.*/command_run.*-case).
func TestEventCadence_VoyageTerminals(t *testing.T) {
	const cid = "cd_x"

	withCadence := []audit.EventType{
		audit.EventScenarioRunCompleted,
		audit.EventScenarioRunFailed,
		audit.EventScenarioRunPartialFailed,
		audit.EventCommandRunCompleted,
		audit.EventCommandRunFailed,
		audit.EventCommandRunPartialFailed,
	}
	for _, et := range withCadence {
		if got := eventCadence(et, map[string]any{"cadence_id": cid}); got != cid {
			t.Fatalf("eventCadence(%s) = %q, want %q", et, got, cid)
		}
		// Без cadence_id (ручной Voyage) → "".
		if got := eventCadence(et, map[string]any{}); got != "" {
			t.Fatalf("eventCadence(%s) без cadence_id = %q, want empty", et, got)
		}
	}

	// Регресс: incarnation.run_completed (task-путь, §k/§l) по-прежнему возвращает cadence_id.
	if got := eventCadence(audit.EventIncarnationRunCompleted, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(incarnation.run_completed) сломан: = %q, want %q", got, cid)
	}
	// Регресс: cadence.spawned / cadence.skipped_overlap не сломаны.
	if got := eventCadence(audit.EventCadenceSpawned, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(cadence.spawned) сломан: = %q, want %q", got, cid)
	}
	if got := eventCadence(audit.EventCadenceSkippedOverlap, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(cadence.skipped_overlap) сломан: = %q, want %q", got, cid)
	}
	// Прочее (drift_checked) cadence_id не несёт → "".
	if got := eventCadence(audit.EventIncarnationDriftChecked, map[string]any{"cadence_id": cid}); got != "" {
		t.Fatalf("eventCadence(drift_checked) = %q, want empty (не cadence-событие)", got)
	}
}

// TestMatchTask_VoyageTerminalRegress — РЕГРЕСС task-селектора: Voyage-терминал
// scenario_run.* (даже с cadence_id) task-селектором НЕ матчится — task-адрес
// осмысленен только для incarnation.run_completed (Voyage-терминал changed_tasks
// не несёт). Добавление cadence_id в Voyage-терминал не должно «оживить» task-путь.
func TestMatchTask_VoyageTerminalRegress(t *testing.T) {
	terminal := scenarioTerminal(map[string]any{"cadence_id": "cd_x"})
	if matchTask(strPtr("nginx_pkg"), audit.EventScenarioRunFailed, terminal) {
		t.Fatal("task-селектор НЕ должен матчить Voyage-терминал scenario_run.* (нет changed_tasks)")
	}
	if matchTask(strPtr("nginx_pkg"), audit.EventCommandRunFailed, commandTerminal(nil)) {
		t.Fatal("task-селектор НЕ должен матчить command_run.* терминал")
	}

	// incarnation.run_completed task-путь не сломан: адрес в changed_tasks матчится.
	runCompleted := runCompletedPayload([]map[string]any{{"register": "nginx_pkg"}}, nil)
	if !matchTask(strPtr("nginx_pkg"), audit.EventIncarnationRunCompleted, runCompleted) {
		t.Fatal("task-путь incarnation.run_completed сломан: адрес в changed_tasks не матчится")
	}
}
