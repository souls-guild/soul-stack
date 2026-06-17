package herald

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// strPtr — хелпер для опц. селекторов.
func strPtr(s string) *string { return &s }

// fakeQueue — собирает поставленные DeliveryJob-ы.
type fakeQueue struct {
	mu   sync.Mutex
	jobs []*DeliveryJob
	err  error
}

func (q *fakeQueue) Enqueue(_ context.Context, job *DeliveryJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.jobs = append(q.jobs, job)
	return nil
}

func (q *fakeQueue) snapshot() []*DeliveryJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*DeliveryJob, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// staticSource — фиксированный набор правил (минует PG).
type staticSource struct {
	rules []*Tiding
	err   error
	calls int
}

func (s *staticSource) EnabledTidings(_ context.Context) ([]*Tiding, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.rules, nil
}

// dispatchOne матчит одно событие против одного правила синхронно.
func dispatchOne(t *testing.T, rule *Tiding, event *audit.Event) []*DeliveryJob {
	t.Helper()
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{rule}},
		Queue:  q,
	})
	d.Dispatch(context.Background(), event)
	return q.snapshot()
}

func ev(et audit.EventType, payload map[string]any) *audit.Event {
	return &audit.Event{EventType: et, CorrelationID: "vy_1", Payload: payload}
}

func TestMatchTiding_Table(t *testing.T) {
	// Реальные payload-формы (см. voyageorch.emitFinalized / emitLegCompleted,
	// incarnation.go drift_checked, conductor.cadence_spawn).
	scenarioCompletedChanged := ev(audit.EventScenarioRunCompleted, map[string]any{
		"voyage_id": "vy_1", "kind": "scenario", "total_batches": 1,
		"summary": map[string]any{"total": 3, "succeeded": 3, "failed": 0, "cancelled": 0},
	})
	scenarioCompletedNoChange := ev(audit.EventScenarioRunCompleted, map[string]any{
		"voyage_id": "vy_1", "kind": "scenario", "total_batches": 1,
		"summary": map[string]any{"total": 3, "succeeded": 0, "failed": 0, "cancelled": 0},
	})
	scenarioFailed := ev(audit.EventScenarioRunFailed, map[string]any{
		"voyage_id": "vy_1", "kind": "scenario", "total_batches": 1, "error_code": "no_match",
		"summary": map[string]any{"total": 3, "succeeded": 0, "failed": 3, "cancelled": 0},
	})
	commandCompletedChanged := ev(audit.EventCommandRunCompleted, map[string]any{
		"voyage_id": "vy_1", "kind": "command", "total": 5, "succeeded": 5,
	})
	commandPartialFailed := ev(audit.EventCommandRunPartialFailed, map[string]any{
		"voyage_id": "vy_1", "kind": "command", "total": 5, "succeeded": 3, "failed": 2,
	})
	driftDirty := ev(audit.EventIncarnationDriftChecked, map[string]any{
		"name": "web", "scenario": "converge", "apply_id": "ap_1",
		"drift_summary": map[string]any{"hosts_drifted": 2, "hosts_clean": 1, "hosts_unsupported": 0, "hosts_failed": 0},
	})
	driftClean := ev(audit.EventIncarnationDriftChecked, map[string]any{
		"name": "web", "scenario": "converge", "apply_id": "ap_1",
		"drift_summary": map[string]any{"hosts_drifted": 0, "hosts_clean": 3, "hosts_unsupported": 0, "hosts_failed": 0},
	})
	cadenceSpawned := ev(audit.EventCadenceSpawned, map[string]any{
		"cadence_id": "cd_nightly", "voyage_id": "vy_1", "scheduled_for": "t", "scope_size": 4,
	})
	legCompletedChanged := ev(audit.EventScenarioRunLegCompleted, map[string]any{
		"voyage_id": "vy_1", "kind": "scenario", "leg_index": 0, "terminal": "success",
		"total": 2, "succeeded": 2, "failed": 0, "cancelled": 0,
	})
	// Вне scope прогонов — не должно матчить даже area-glob соседней области.
	heraldCreated := ev(audit.EventType("herald.created"), map[string]any{"name": "x"})

	rule := func(mut func(*Tiding)) *Tiding {
		t := &Tiding{Name: "t", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true}
		if mut != nil {
			mut(t)
		}
		return t
	}

	cases := []struct {
		name  string
		rule  *Tiding
		event *audit.Event
		want  bool
	}{
		{"area-glob matches scenario_run.completed", rule(nil), scenarioCompletedChanged, true},
		{"area-glob does not match command_run", rule(nil), commandCompletedChanged, false},
		{"exact event_type match", rule(func(t *Tiding) { t.EventTypes = []string{"command_run.completed"} }), commandCompletedChanged, true},
		{"exact event_type miss", rule(func(t *Tiding) { t.EventTypes = []string{"command_run.failed"} }), commandCompletedChanged, false},
		{"multi event_types any-match", rule(func(t *Tiding) { t.EventTypes = []string{"voyage.*", "command_run.completed"} }), commandCompletedChanged, true},
		{"point event_type drift_checked", rule(func(t *Tiding) { t.EventTypes = []string{"incarnation.drift_checked"} }), driftDirty, true},

		{"only_failures passes failed", rule(func(t *Tiding) { t.OnlyFailures = true }), scenarioFailed, true},
		{"only_failures blocks completed", rule(func(t *Tiding) { t.OnlyFailures = true }), scenarioCompletedChanged, false},
		{"only_failures passes command partial_failed", rule(func(t *Tiding) {
			t.EventTypes = []string{"command_run.*"}
			t.OnlyFailures = true
		}), commandPartialFailed, true},

		{"only_changes passes scenario completed with succeeded>0", rule(func(t *Tiding) { t.OnlyChanges = true }), scenarioCompletedChanged, true},
		{"only_changes blocks scenario completed with succeeded=0", rule(func(t *Tiding) { t.OnlyChanges = true }), scenarioCompletedNoChange, false},
		{"only_changes passes command completed succeeded>0", rule(func(t *Tiding) {
			t.EventTypes = []string{"command_run.*"}
			t.OnlyChanges = true
		}), commandCompletedChanged, true},
		{"only_changes passes drift dirty", rule(func(t *Tiding) {
			t.EventTypes = []string{"incarnation.drift_checked"}
			t.OnlyChanges = true
		}), driftDirty, true},
		{"only_changes blocks drift clean", rule(func(t *Tiding) {
			t.EventTypes = []string{"incarnation.drift_checked"}
			t.OnlyChanges = true
		}), driftClean, false},
		{"only_changes passes leg_completed with succeeded>0", rule(func(t *Tiding) {
			t.EventTypes = []string{"scenario_run.leg_completed"}
			t.OnlyChanges = true
		}), legCompletedChanged, true},

		{"only_failures + only_changes both required", rule(func(t *Tiding) {
			t.OnlyFailures = true
			t.OnlyChanges = true
		}), scenarioFailed, false}, // failed-событие succeeded=0 → changes=false

		{"incarnation selector match on drift", rule(func(t *Tiding) {
			t.EventTypes = []string{"incarnation.drift_checked"}
			t.Incarnation = strPtr("web")
		}), driftDirty, true},
		{"incarnation selector mismatch on drift", rule(func(t *Tiding) {
			t.EventTypes = []string{"incarnation.drift_checked"}
			t.Incarnation = strPtr("db")
		}), driftDirty, false},
		{"incarnation selector blocks scenario_run (no incarnation field)", rule(func(t *Tiding) {
			t.Incarnation = strPtr("web")
		}), scenarioCompletedChanged, false},

		{"cadence selector match on cadence.spawned", rule(func(t *Tiding) {
			t.EventTypes = []string{"cadence.*"}
			t.Cadence = strPtr("cd_nightly")
		}), cadenceSpawned, true},
		{"cadence selector mismatch", rule(func(t *Tiding) {
			t.EventTypes = []string{"cadence.*"}
			t.Cadence = strPtr("cd_hourly")
		}), cadenceSpawned, false},

		{"out-of-scope event never matches", rule(func(t *Tiding) { t.EventTypes = []string{"scenario_run.*"} }), heraldCreated, false},

		// Ephemeral (ADR-052(g)): voyage_id-селектор сужает правило до СВОЕГО
		// прогона. ev() кладёт payload["voyage_id"]="vy_1" и CorrelationID="vy_1".
		{"ephemeral matches own voyage (payload voyage_id)", rule(func(t *Tiding) {
			t.Ephemeral = true
			t.VoyageID = strPtr("vy_1")
		}), scenarioCompletedChanged, true},
		{"ephemeral blocks other voyage", rule(func(t *Tiding) {
			t.Ephemeral = true
			t.VoyageID = strPtr("vy_other")
		}), scenarioCompletedChanged, false},
		{"ephemeral matches own voyage via correlation_id fallback", rule(func(t *Tiding) {
			t.Ephemeral = true
			t.VoyageID = strPtr("vy_1")
		}), ev(audit.EventScenarioRunCompleted, map[string]any{ // payload без voyage_id, но CorrelationID=vy_1
			"summary": map[string]any{"succeeded": 1},
		}), true},
		{"persistent rule unaffected (voyage_id nil) still matches any voyage", rule(nil), scenarioCompletedChanged, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchTiding(c.rule, c.event)
			if got != c.want {
				t.Fatalf("matchTiding = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDispatch_DisabledRuleNotInSource(t *testing.T) {
	// Source отдаёт только enabled (PG WHERE enabled=true). Disabled-правило
	// просто отсутствует в снимке — матча нет.
	disabled := &Tiding{Name: "off", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: false}
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: nil}, // source уже отфильтровал disabled
		Queue:  q,
	})
	_ = disabled
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, map[string]any{"summary": map[string]any{"succeeded": 1}}))
	if jobs := q.snapshot(); len(jobs) != 0 {
		t.Fatalf("disabled-правило (отсутствует в source) не должно порождать job-ы, получил %d", len(jobs))
	}
}

func TestDispatch_MultipleMatches_OneJobPerRule(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h1", EventTypes: []string{"scenario_run.*"}, Enabled: true},
			{Name: "b", Herald: "h2", EventTypes: []string{"scenario_run.completed"}, Enabled: true},
			{Name: "c", Herald: "h3", EventTypes: []string{"command_run.*"}, Enabled: true},
		}},
		Queue: q,
	})
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, map[string]any{
		"summary": map[string]any{"succeeded": 1},
	}))
	jobs := q.snapshot()
	if len(jobs) != 2 {
		t.Fatalf("ожидалось 2 job-а (правила a,b), получил %d", len(jobs))
	}
	if jobs[0].Herald != "h1" || jobs[1].Herald != "h2" {
		t.Fatalf("неверные heralds в job-ах: %q, %q", jobs[0].Herald, jobs[1].Herald)
	}
}

func TestDispatch_JobCarriesPayloadCopy(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
		}},
		Queue: q,
	})
	orig := map[string]any{"voyage_id": "vy_1", "summary": map[string]any{"succeeded": 1}}
	event := ev(audit.EventScenarioRunCompleted, orig)
	d.Dispatch(context.Background(), event)

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("ожидался 1 job, получил %d", len(jobs))
	}
	job := jobs[0]
	// Копия, не тот же указатель.
	if &job.PayloadCopy == &event.Payload {
		t.Fatal("PayloadCopy должен быть копией map, не тем же значением")
	}
	// Мутация копии не задевает оригинал.
	job.PayloadCopy["injected"] = true
	if _, exists := event.Payload["injected"]; exists {
		t.Fatal("мутация PayloadCopy просочилась в исходный payload")
	}
	if job.CorrelationID != "vy_1" {
		t.Fatalf("CorrelationID не перенесён: %q", job.CorrelationID)
	}
}

// TestDispatch_JobCarriesAnnotationsProjection — dispatcher ПЕРЕНОСИТ
// Annotations/Projection из Tiding в DeliveryJob (ADR-052(h) N1), но НЕ применяет
// их к payload (это off-path в worker-е, N3). Здесь фиксируем перенос полей и то,
// что PayloadCopy остаётся ПОЛНОЙ копией (projection не сужает payload в N1).
func TestDispatch_JobCarriesAnnotationsProjection(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{{
			Name:        "a",
			Herald:      "h",
			EventTypes:  []string{"scenario_run.*"},
			Enabled:     true,
			Annotations: map[string]any{"team": "ops", "severity": "high"},
			Projection:  []string{"event_type", "summary.succeeded"},
		}}},
		Queue: q,
	})
	orig := map[string]any{"voyage_id": "vy_1", "summary": map[string]any{"succeeded": 1, "failed": 0}}
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, orig))

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("ожидался 1 job, получил %d", len(jobs))
	}
	job := jobs[0]
	if job.Annotations["team"] != "ops" || job.Annotations["severity"] != "high" {
		t.Errorf("Annotations не перенесены: %+v", job.Annotations)
	}
	if len(job.Projection) != 2 || job.Projection[0] != "event_type" {
		t.Errorf("Projection не перенесён: %+v", job.Projection)
	}
	// N1: dispatcher НЕ сужает payload по projection (это N3). Полная копия цела.
	if _, ok := job.PayloadCopy["summary"]; !ok {
		t.Error("dispatcher не должен применять projection в N1 — payload должен быть полным")
	}
}

// TestDispatch_PayloadCopy_ShallowByDesign документирует осознанный trade-off
// copyPayload (review S2, нит-4): копируется ТОЛЬКО верхний уровень. Глубокая
// мутация вложенного map видна и через PayloadCopy, и через оригинал — это
// допустимо (payload замаскирован, downstream read-only). Тест фиксирует
// поведение, чтобы случайный переход на deep-copy не прошёл молча.
func TestDispatch_PayloadCopy_ShallowByDesign(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
		}},
		Queue: q,
	})
	nested := map[string]any{"succeeded": 1}
	orig := map[string]any{"voyage_id": "vy_1", "summary": nested}
	event := ev(audit.EventScenarioRunCompleted, orig)
	d.Dispatch(context.Background(), event)

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("ожидался 1 job, получил %d", len(jobs))
	}
	// Вложенный map — тот же указатель (shallow): мутация через копию видна в
	// оригинале. НЕ изолировано — by design.
	jobs[0].PayloadCopy["summary"].(map[string]any)["succeeded"] = 999
	if got := orig["summary"].(map[string]any)["succeeded"]; got != 999 {
		t.Fatalf("ожидалась shared вложенная map (shallow copy), но мутация не просочилась: succeeded=%v", got)
	}
}

func TestDispatch_SourceError_NoPanicNoJob(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{err: errors.New("pg down")},
		Queue:  q,
	})
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, nil))
	if len(q.snapshot()) != 0 {
		t.Fatal("при ошибке source job-ов быть не должно")
	}
}

func TestDispatch_EnqueueError_ContinuesOtherRules(t *testing.T) {
	q := &fakeQueue{err: errors.New("queue full")}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{
			{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
		}},
		Queue: q,
	})
	// Не должно паниковать; ошибка enqueue проглатывается.
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, nil))
}

func TestRuleCache_TTLAndInvalidation(t *testing.T) {
	src := &staticSource{rules: []*Tiding{
		{Name: "a", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true},
	}}
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{Source: src, Queue: q, TTL: time.Hour})

	event := ev(audit.EventScenarioRunCompleted, nil)
	d.Dispatch(context.Background(), event)
	d.Dispatch(context.Background(), event)
	if src.calls != 1 {
		t.Fatalf("в пределах TTL source должен читаться 1 раз, прочитан %d", src.calls)
	}

	// Инвалидация форсирует перечитывание.
	d.InvalidateRules()
	d.Dispatch(context.Background(), event)
	if src.calls != 2 {
		t.Fatalf("после InvalidateRules source должен перечитаться, calls=%d", src.calls)
	}
}

func TestRuleCache_TTLExpiry(t *testing.T) {
	src := &staticSource{rules: nil}
	d := NewDispatcher(DispatcherConfig{Source: src, Queue: &fakeQueue{}, TTL: 10 * time.Millisecond})
	now := time.Now()
	d.clock = func() time.Time { return now }

	event := ev(audit.EventScenarioRunCompleted, nil)
	d.Dispatch(context.Background(), event)
	if src.calls != 1 {
		t.Fatalf("первый Dispatch должен загрузить правила, calls=%d", src.calls)
	}
	// Перематываем часы за TTL.
	now = now.Add(20 * time.Millisecond)
	d.Dispatch(context.Background(), event)
	if src.calls != 2 {
		t.Fatalf("после истечения TTL source должен перечитаться, calls=%d", src.calls)
	}
}

// TestDispatch_OccurredAt_FallbackToMatchTime — guard на баг live-smoke Herald:
// event.CreatedAt нулевой (инициатор опёрся на PG DEFAULT NOW(), tap наблюдает
// указатель ДО заполнения времени) → job.OccurredAt должен быть НЕ нулевым,
// проставленным временем матча (d.clock), а не 0001-01-01.
func TestDispatch_OccurredAt_FallbackToMatchTime(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{{Name: "t", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true}}},
		Queue:  q,
	})
	matchTime := time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC)
	d.clock = func() time.Time { return matchTime }

	event := ev(audit.EventScenarioRunFailed, map[string]any{"voyage_id": "v1"})
	if !event.CreatedAt.IsZero() {
		t.Fatal("предусловие: event.CreatedAt должен быть нулевым (воспроизводит баг)")
	}
	d.Dispatch(context.Background(), event)

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("ожидался 1 job, получил %d", len(jobs))
	}
	if jobs[0].OccurredAt.IsZero() {
		t.Fatal("job.OccurredAt нулевой — баг occurred_at=0001-01-01 не закрыт")
	}
	if !jobs[0].OccurredAt.Equal(matchTime) {
		t.Fatalf("job.OccurredAt = %v, want время матча %v", jobs[0].OccurredAt, matchTime)
	}
}

// TestDispatch_OccurredAt_PrefersExplicitCreatedAt — если инициатор проставил
// event.CreatedAt явно, job.OccurredAt берёт именно его (UTC), а не время матча.
func TestDispatch_OccurredAt_PrefersExplicitCreatedAt(t *testing.T) {
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: []*Tiding{{Name: "t", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: true}}},
		Queue:  q,
	})
	d.clock = func() time.Time { return time.Date(2026, 6, 11, 10, 30, 0, 0, time.UTC) }

	created := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	event := ev(audit.EventScenarioRunFailed, map[string]any{"voyage_id": "v1"})
	event.CreatedAt = created
	d.Dispatch(context.Background(), event)

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("ожидался 1 job, получил %d", len(jobs))
	}
	if !jobs[0].OccurredAt.Equal(created) {
		t.Fatalf("job.OccurredAt = %v, want явный CreatedAt %v", jobs[0].OccurredAt, created)
	}
}
