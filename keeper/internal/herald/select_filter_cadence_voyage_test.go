package herald

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// scenarioTerminal is a kind=scenario Voyage terminal payload as written by
// voyageorch.emitFinalized. extra (cadence_id) is added when run.CadenceID != nil.
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

// commandTerminal is a kind=command Voyage terminal payload (emitFinalized).
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

// TestDispatch_CadenceNotifyDelivery is the main guard (ADR-052(l) amendment,
// option b): its absence let a QA blocker pass. A cadence-notify rule
// (event_types=[scenario_run.failed], cadence=<ULID>, created_from_cadence_id=<ULID>)
// against a Voyage terminal scenario_run.failed with payload cadence_id=<ULID> must
// reach delivery: the full dispatcher matchTiding path, not only InsertTiding.
func TestDispatch_CadenceNotifyDelivery(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	failedTerminal := ev(audit.EventScenarioRunFailed, scenarioTerminal(map[string]any{
		"cadence_id": cadenceULID,
		"error_code": "no_match",
		"summary":    map[string]any{"total": 3, "succeeded": 0, "failed": 3, "cancelled": 0},
	}))

	// The rule is exactly as cadence.notify creates it. created_from_cadence_id is
	// an origin marker and does not affect matching, but the production rule shape
	// is fixed here.
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
		t.Fatalf("cadence-notify rule must deliver exactly 1 job for a Voyage terminal "+
			"scenario_run.failed with cadence_id, got %d (QA blocker: rule did not match)", len(jobs))
	}
}

// TestDispatch_CadenceNotify_AllTerminals verifies that completed/failed/
// partial_failed Voyage terminals scenario_run.* with cadence_id match a cadence
// rule by the corresponding event_type.
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
				t.Fatalf("cadence rule for %s must deliver 1 job, got %d", c.et, len(jobs))
			}
		})
	}
}

// TestDispatch_CadenceNotify_PartialGlob verifies that a cadence rule on:[partial]
// through area-glob scenario_run.* matches scenario_run.partial_failed with
// cadence_id.
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
		t.Fatalf("cadence rule scenario_run.* must deliver 1 job for partial_failed with cadence_id, got %d", len(jobs))
	}
}

// TestDispatch_CadenceNotify_CommandTerminal verifies that a command Voyage
// (cadence may spawn a command run) also carries cadence_id at the terminal and
// matches.
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
		t.Fatalf("cadence rule command_run.* must deliver 1 job for command terminal with cadence_id, got %d", len(jobs))
	}
}

// TestDispatch_CadenceNotify_ManualVoyageNotMatched verifies that a manual Voyage
// terminal without cadence_id does not match a cadence rule, conservatively.
func TestDispatch_CadenceNotify_ManualVoyageNotMatched(t *testing.T) {
	const cadenceULID = "01JZZZZ0CADENCE000000000000"

	manual := ev(audit.EventScenarioRunFailed, scenarioTerminal(nil)) // Without cadence_id.
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"scenario_run.failed"},
		Cadence:    strPtr(cadenceULID),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, manual); len(jobs) != 0 {
		t.Fatalf("cadence rule must not match a manual Voyage terminal without cadence_id, got %d job", len(jobs))
	}
}

// TestDispatch_CadenceNotify_MismatchedID verifies that another terminal cadence_id
// does not match.
func TestDispatch_CadenceNotify_MismatchedID(t *testing.T) {
	other := ev(audit.EventScenarioRunFailed, scenarioTerminal(map[string]any{"cadence_id": "cd_other"}))
	rule := &Tiding{
		Name: "cadence-notify", Herald: "ops",
		EventTypes: []string{"scenario_run.failed"},
		Cadence:    strPtr("cd_mine"),
		Enabled:    true,
	}
	if jobs := dispatchOne(t, rule, other); len(jobs) != 0 {
		t.Fatalf("cadence rule must not match a terminal with a foreign cadence_id, got %d job", len(jobs))
	}
}

// TestEventCadence_VoyageTerminals verifies that eventCadence extracts cadence_id
// from all six Voyage terminals and still from cadence.*/incarnation.run_completed
// after adding the scenario_run.*/command_run.* case.
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
		// Without cadence_id (manual Voyage): "".
		if got := eventCadence(et, map[string]any{}); got != "" {
			t.Fatalf("eventCadence(%s) without cadence_id = %q, want empty", et, got)
		}
	}

	// Regression: incarnation.run_completed (task path, ADR-052(k)/(l)) still returns cadence_id.
	if got := eventCadence(audit.EventIncarnationRunCompleted, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(incarnation.run_completed) broken: = %q, want %q", got, cid)
	}
	// Regression: cadence.spawned / cadence.skipped_overlap are still intact.
	if got := eventCadence(audit.EventCadenceSpawned, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(cadence.spawned) broken: = %q, want %q", got, cid)
	}
	if got := eventCadence(audit.EventCadenceSkippedOverlap, map[string]any{"cadence_id": cid}); got != cid {
		t.Fatalf("eventCadence(cadence.skipped_overlap) broken: = %q, want %q", got, cid)
	}
	// Other events (drift_checked) do not carry cadence_id: "".
	if got := eventCadence(audit.EventIncarnationDriftChecked, map[string]any{"cadence_id": cid}); got != "" {
		t.Fatalf("eventCadence(drift_checked) = %q, want empty (not a cadence event)", got)
	}
}

// TestMatchTask_VoyageTerminalRegress is a task selector regression: Voyage
// terminal scenario_run.* does not match the task selector, even with cadence_id.
// The task address is meaningful only for incarnation.run_completed; Voyage
// terminals do not carry changed_tasks. Adding cadence_id to a Voyage terminal
// must not revive the task path.
func TestMatchTask_VoyageTerminalRegress(t *testing.T) {
	terminal := scenarioTerminal(map[string]any{"cadence_id": "cd_x"})
	if matchTask(strPtr("nginx_pkg"), audit.EventScenarioRunFailed, terminal) {
		t.Fatal("task selector must not match Voyage terminal scenario_run.* (no changed_tasks)")
	}
	if matchTask(strPtr("nginx_pkg"), audit.EventCommandRunFailed, commandTerminal(nil)) {
		t.Fatal("task selector must not match command_run.* terminal")
	}

	// incarnation.run_completed task path is still intact: address in changed_tasks matches.
	runCompleted := runCompletedPayload([]map[string]any{{"register": "nginx_pkg"}}, nil)
	if !matchTask(strPtr("nginx_pkg"), audit.EventIncarnationRunCompleted, runCompleted) {
		t.Fatal("task path incarnation.run_completed is broken: address in changed_tasks does not match")
	}
}
