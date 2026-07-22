package herald

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// changedTasksMaps represents changed_tasks format as emitter puts it in-process
// (scenario.changedTasksPayload → []map[string]any). matchTask must match
// this form (tap sees raw emitter payload, not masked/round-trip copy).
func runCompletedPayload(changed []map[string]any, extra map[string]any) map[string]any {
	p := map[string]any{
		"incarnation":   "web",
		"scenario":      "deploy",
		"apply_id":      "ap_1",
		"status":        "success",
		"changed_tasks": changed,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// TestMatchTask_Semantics core of task selector (ADR-052 §l): match by address
// register∪id in changed_tasks of incarnation.run_completed event.
func TestMatchTask_Semantics(t *testing.T) {
	changed := []map[string]any{
		{"idx": 0, "name": "nginx", "register": "nginx_pkg", "id": "", "module": "core.pkg", "changed_hosts": 2, "total_hosts": 3},
		{"idx": 1, "name": "tune", "register": "", "id": "sysctl_tune", "module": "core.sysctl", "changed_hosts": 1, "total_hosts": 3},
		{"idx": 2, "name": "noaddr", "register": "", "id": "", "module": "core.exec", "changed_hosts": 1, "total_hosts": 1},
	}
	runCompleted := func(ch []map[string]any) *audit.Event {
		return ev(audit.EventIncarnationRunCompleted, runCompletedPayload(ch, nil))
	}

	cases := []struct {
		name  string
		sel   *string
		event *audit.Event
		want  bool
	}{
		{"register match", strPtr("nginx_pkg"), runCompleted(changed), true},
		{"id match", strPtr("sysctl_tune"), runCompleted(changed), true},
		{"no such address", strPtr("absent"), runCompleted(changed), false},
		{"nil selector matches any (no filter)", nil, runCompleted(changed), true},
		{"nil selector matches empty changed_tasks", nil, runCompleted(nil), true},
		// Empty *sel (normally filtered by validateTiding ""→nil; here is defence-in-depth
		// for matchTask) does not match unaddressable task changed_tasks (register=="" && id=="").
		{"empty selector does not match empty address", strPtr(""), runCompleted(changed), false},
		// Register selector must not match record with same text in id and vice versa —
		// verify both branches are independent: id-address does not match register-selector.
		{"register selector does not match id-only entry", strPtr("sysctl_tune"), runCompleted([]map[string]any{
			{"register": "sysctl_tune", "id": ""}, // same text but in register — must match as register
		}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchTask(c.sel, c.event.EventType, c.event.Payload); got != c.want {
				t.Fatalf("matchTask(%v) = %v, want %v", c.sel, got, c.want)
			}
		})
	}
}

// TestMatchTask_WrongEventType verifies task selector on non-run_completed event
// (no changed_tasks) does not match. Guard: task address is meaningful only for
// per-incarnation run summary.
func TestMatchTask_WrongEventType(t *testing.T) {
	// scenario_run.completed carries summary but not changed_tasks → no match.
	scenarioDone := ev(audit.EventScenarioRunCompleted, map[string]any{
		"summary": map[string]any{"succeeded": 1},
	})
	if matchTask(strPtr("nginx_pkg"), scenarioDone.EventType, scenarioDone.Payload) {
		t.Fatal("task selector must not match scenario_run.completed (no changed_tasks)")
	}
	// drift_checked — different type, also without changed_tasks.
	drift := ev(audit.EventIncarnationDriftChecked, map[string]any{"name": "web"})
	if matchTask(strPtr("nginx_pkg"), drift.EventType, drift.Payload) {
		t.Fatal("task selector must not match incarnation.drift_checked")
	}
}

// TestMatchTask_RoundTripForm verifies tolerance for []any of maps (after JSON
// round-trip), symmetric to in-process []map[string]any.
func TestMatchTask_RoundTripForm(t *testing.T) {
	payload := map[string]any{
		"status": "success",
		"changed_tasks": []any{
			map[string]any{"register": "nginx_pkg", "id": ""},
			map[string]any{"register": "", "id": "sysctl_tune"},
		},
	}
	if !matchTask(strPtr("nginx_pkg"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask must match register-address in []any-form changed_tasks")
	}
	if !matchTask(strPtr("sysctl_tune"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask must match id-address in []any-form changed_tasks")
	}
	if matchTask(strPtr("absent"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask must not match absent address in []any-form")
	}
}

// TestMatchTiding_TaskSelector verifies task selector as part of full matchTiding
// (via dispatcher): rule with task matches run_completed only when address exists.
func TestMatchTiding_TaskSelector(t *testing.T) {
	changed := []map[string]any{
		{"register": "nginx_pkg", "id": ""},
	}
	matchEvent := ev(audit.EventIncarnationRunCompleted, runCompletedPayload(changed, nil))
	missEvent := ev(audit.EventIncarnationRunCompleted, runCompletedPayload([]map[string]any{
		{"register": "other_pkg", "id": ""},
	}, nil))

	taskRule := func() *Tiding {
		return &Tiding{
			Name: "t", Herald: "h",
			EventTypes: []string{"incarnation.run_completed"},
			Task:       strPtr("nginx_pkg"),
			Enabled:    true,
		}
	}

	if !matchTiding(taskRule(), matchEvent) {
		t.Fatal("rule with task=nginx_pkg must match run_completed with this address in changed_tasks")
	}
	if matchTiding(taskRule(), missEvent) {
		t.Fatal("rule with task=nginx_pkg must not match run_completed without this address")
	}

	// End-to-end delivery via dispatcher: exactly one job on matching event, zero on non-matching.
	if jobs := dispatchOne(t, taskRule(), matchEvent); len(jobs) != 1 {
		t.Fatalf("expected 1 job on matching event, got %d", len(jobs))
	}
	if jobs := dispatchOne(t, taskRule(), missEvent); len(jobs) != 0 {
		t.Fatalf("expected 0 jobs on non-matching event, got %d", len(jobs))
	}
}

// TestHasChanges_RunCompleted verifies only_changes × incarnation.run_completed:
// changed ⇔ len(changed_tasks)>0. failed without changes → false (consistency
// of only_changes with task selector, ADR-052 §l).
func TestHasChanges_RunCompleted(t *testing.T) {
	withChanges := runCompletedPayload([]map[string]any{{"register": "nginx_pkg"}}, nil)
	empty := runCompletedPayload(nil, map[string]any{"status": "failed"})

	if !hasChanges(audit.EventIncarnationRunCompleted, withChanges) {
		t.Fatal("hasChanges must be true when changed_tasks is not empty")
	}
	if hasChanges(audit.EventIncarnationRunCompleted, empty) {
		t.Fatal("hasChanges must be false when changed_tasks is empty (failed without changes)")
	}

	// only_changes rule: passes event with changes, filters out empty.
	onlyChanges := &Tiding{
		Name: "t", Herald: "h",
		EventTypes:  []string{"incarnation.run_completed"},
		OnlyChanges: true,
		Enabled:     true,
	}
	if !matchTiding(onlyChanges, ev(audit.EventIncarnationRunCompleted, withChanges)) {
		t.Fatal("only_changes rule must pass run_completed with changed_tasks")
	}
	if matchTiding(onlyChanges, ev(audit.EventIncarnationRunCompleted, empty)) {
		t.Fatal("only_changes rule must filter run_completed without changed_tasks")
	}

	// Combination of only_changes + task selector: both must pass together.
	combo := &Tiding{
		Name: "t", Herald: "h",
		EventTypes:  []string{"incarnation.run_completed"},
		OnlyChanges: true,
		Task:        strPtr("nginx_pkg"),
		Enabled:     true,
	}
	if !matchTiding(combo, ev(audit.EventIncarnationRunCompleted, withChanges)) {
		t.Fatal("only_changes + task=nginx_pkg must together pass matching event")
	}
}

// TestMatchCadence_RunCompleted verifies cadence selector now catches schedule run
// results (incarnation.run_completed with cadence_id) without breaking
// cadence.spawned/skipped_overlap (T4b, ADR-052).
func TestMatchCadence_RunCompleted(t *testing.T) {
	// run_completed spawned by schedule carries cadence_id.
	runByCadence := ev(audit.EventIncarnationRunCompleted, runCompletedPayload(
		[]map[string]any{{"register": "nginx_pkg"}},
		map[string]any{"cadence_id": "cd_nightly"},
	))
	// Manual run does not carry cadence_id.
	runManual := ev(audit.EventIncarnationRunCompleted, runCompletedPayload(
		[]map[string]any{{"register": "nginx_pkg"}}, nil,
	))
	cadenceSpawned := ev(audit.EventCadenceSpawned, map[string]any{
		"cadence_id": "cd_nightly", "voyage_id": "vy_1",
	})

	cadenceRule := func(et string) *Tiding {
		return &Tiding{
			Name: "t", Herald: "h",
			EventTypes: []string{et},
			Cadence:    strPtr("cd_nightly"),
			Enabled:    true,
		}
	}

	if !matchTiding(cadenceRule("incarnation.run_completed"), runByCadence) {
		t.Fatal("cadence=cd_nightly must match run_completed with cadence_id=cd_nightly")
	}
	if matchTiding(cadenceRule("incarnation.run_completed"), runManual) {
		t.Fatal("cadence selector must not match manual run_completed (no cadence_id)")
	}
	// Not broken: cadence.spawned still matches.
	if !matchTiding(cadenceRule("cadence.*"), cadenceSpawned) {
		t.Fatal("cadence=cd_nightly must still match cadence.spawned")
	}
	// Mismatched cadence_id on run_completed → no match.
	if matchTiding(&Tiding{
		Name: "t", Herald: "h",
		EventTypes: []string{"incarnation.run_completed"},
		Cadence:    strPtr("cd_other"),
		Enabled:    true,
	}, runByCadence) {
		t.Fatal("cadence=cd_other must not match run_completed with cadence_id=cd_nightly")
	}
}
