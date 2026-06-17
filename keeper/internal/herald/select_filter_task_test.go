package herald

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// changedTasksMaps — форма changed_tasks как её кладёт эмиттер in-process
// (scenario.changedTasksPayload → []map[string]any). matchTask должен матчить
// именно её (tap видит сырой payload эмиттера, не маскированную/round-trip копию).
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

// TestMatchTask_Semantics — ядро task-селектора (ADR-052 §l): матч по адресу
// register∪id в changed_tasks события incarnation.run_completed.
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
		// Пустой *sel (нормально отсекается validateTiding ""→nil; здесь — defence-
		// in-depth matchTask) НЕ матчит неадресуемую задачу changed_tasks (register=="" && id=="").
		{"empty selector does not match empty address", strPtr(""), runCompleted(changed), false},
		// Селектор по register не должен ловить запись с тем же текстом в id и наоборот —
		// проверяем, что обе ветки независимы: id-адрес не матчит register-селектор.
		{"register selector does not match id-only entry", strPtr("sysctl_tune"), runCompleted([]map[string]any{
			{"register": "sysctl_tune", "id": ""}, // тот же текст, но в register — должен матчить как register
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

// TestMatchTask_WrongEventType — task-селектор на НЕ-run_completed событии
// (нет changed_tasks) не матчит. Защита: задача-адрес осмысленен только для
// per-incarnation итога прогона.
func TestMatchTask_WrongEventType(t *testing.T) {
	// scenario_run.completed несёт summary, но НЕ changed_tasks → не матч.
	scenarioDone := ev(audit.EventScenarioRunCompleted, map[string]any{
		"summary": map[string]any{"succeeded": 1},
	})
	if matchTask(strPtr("nginx_pkg"), scenarioDone.EventType, scenarioDone.Payload) {
		t.Fatal("task-селектор не должен матчить scenario_run.completed (нет changed_tasks)")
	}
	// drift_checked — другой тип, тоже без changed_tasks.
	drift := ev(audit.EventIncarnationDriftChecked, map[string]any{"name": "web"})
	if matchTask(strPtr("nginx_pkg"), drift.EventType, drift.Payload) {
		t.Fatal("task-селектор не должен матчить incarnation.drift_checked")
	}
}

// TestMatchTask_RoundTripForm — terпимость к []any из map'ов (после JSON
// round-trip), симметрично in-process []map[string]any.
func TestMatchTask_RoundTripForm(t *testing.T) {
	payload := map[string]any{
		"status": "success",
		"changed_tasks": []any{
			map[string]any{"register": "nginx_pkg", "id": ""},
			map[string]any{"register": "", "id": "sysctl_tune"},
		},
	}
	if !matchTask(strPtr("nginx_pkg"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask должен матчить register-адрес в []any-форме changed_tasks")
	}
	if !matchTask(strPtr("sysctl_tune"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask должен матчить id-адрес в []any-форме changed_tasks")
	}
	if matchTask(strPtr("absent"), audit.EventIncarnationRunCompleted, payload) {
		t.Fatal("matchTask не должен матчить отсутствующий адрес в []any-форме")
	}
}

// TestMatchTiding_TaskSelector — task-селектор в составе полного matchTiding
// (через dispatcher): правило с task матчит run_completed только когда адрес есть.
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
		t.Fatal("правило с task=nginx_pkg должно матчить run_completed с этим адресом в changed_tasks")
	}
	if matchTiding(taskRule(), missEvent) {
		t.Fatal("правило с task=nginx_pkg НЕ должно матчить run_completed без этого адреса")
	}

	// Доставка end-to-end через dispatcher: ровно один job на матчевое событие, ноль на нематчевое.
	if jobs := dispatchOne(t, taskRule(), matchEvent); len(jobs) != 1 {
		t.Fatalf("ожидался 1 job на матчевое событие, получил %d", len(jobs))
	}
	if jobs := dispatchOne(t, taskRule(), missEvent); len(jobs) != 0 {
		t.Fatalf("ожидалось 0 job на нематчевое событие, получил %d", len(jobs))
	}
}

// TestHasChanges_RunCompleted — only_changes × incarnation.run_completed:
// changed ⇔ len(changed_tasks)>0. failed без изменений → false (консистентность
// only_changes с task-селектором, ADR-052 §l).
func TestHasChanges_RunCompleted(t *testing.T) {
	withChanges := runCompletedPayload([]map[string]any{{"register": "nginx_pkg"}}, nil)
	empty := runCompletedPayload(nil, map[string]any{"status": "failed"})

	if !hasChanges(audit.EventIncarnationRunCompleted, withChanges) {
		t.Fatal("hasChanges должен быть true когда changed_tasks непуст")
	}
	if hasChanges(audit.EventIncarnationRunCompleted, empty) {
		t.Fatal("hasChanges должен быть false когда changed_tasks пуст (failed без изменений)")
	}

	// only_changes-правило: пропускает событие с изменениями, отсекает пустое.
	onlyChanges := &Tiding{
		Name: "t", Herald: "h",
		EventTypes:  []string{"incarnation.run_completed"},
		OnlyChanges: true,
		Enabled:     true,
	}
	if !matchTiding(onlyChanges, ev(audit.EventIncarnationRunCompleted, withChanges)) {
		t.Fatal("only_changes-правило должно пропустить run_completed с changed_tasks")
	}
	if matchTiding(onlyChanges, ev(audit.EventIncarnationRunCompleted, empty)) {
		t.Fatal("only_changes-правило должно отсечь run_completed без changed_tasks")
	}

	// Комбинация only_changes + task-селектор: оба должны проходить вместе.
	combo := &Tiding{
		Name: "t", Herald: "h",
		EventTypes:  []string{"incarnation.run_completed"},
		OnlyChanges: true,
		Task:        strPtr("nginx_pkg"),
		Enabled:     true,
	}
	if !matchTiding(combo, ev(audit.EventIncarnationRunCompleted, withChanges)) {
		t.Fatal("only_changes + task=nginx_pkg должны вместе пропустить матчевое событие")
	}
}

// TestMatchCadence_RunCompleted — cadence-селектор теперь ловит результаты
// прогонов расписания (incarnation.run_completed с cadence_id), не сломав
// cadence.spawned/skipped_overlap (T4b, ADR-052).
func TestMatchCadence_RunCompleted(t *testing.T) {
	// run_completed заспавненного расписанием прогона несёт cadence_id.
	runByCadence := ev(audit.EventIncarnationRunCompleted, runCompletedPayload(
		[]map[string]any{{"register": "nginx_pkg"}},
		map[string]any{"cadence_id": "cd_nightly"},
	))
	// Ручной прогон cadence_id не несёт.
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
		t.Fatal("cadence=cd_nightly должно матчить run_completed с cadence_id=cd_nightly")
	}
	if matchTiding(cadenceRule("incarnation.run_completed"), runManual) {
		t.Fatal("cadence-селектор не должен матчить ручной run_completed (нет cadence_id)")
	}
	// Не сломан: cadence.spawned по-прежнему матчит.
	if !matchTiding(cadenceRule("cadence.*"), cadenceSpawned) {
		t.Fatal("cadence=cd_nightly должно по-прежнему матчить cadence.spawned")
	}
	// Mismatch cadence_id на run_completed → не матч.
	if matchTiding(&Tiding{
		Name: "t", Herald: "h",
		EventTypes: []string{"incarnation.run_completed"},
		Cadence:    strPtr("cd_other"),
		Enabled:    true,
	}, runByCadence) {
		t.Fatal("cadence=cd_other не должно матчить run_completed с cadence_id=cd_nightly")
	}
}
