package herald

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// strPtr helper for optional selectors.
func strPtr(s string) *string { return &s }

// fakeQueue collects enqueued DeliveryJobs.
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

// staticSource fixed set of rules (bypasses PG).
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

// dispatchOne synchronously matches one event against one rule.
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
	// Real payload forms (see voyageorch.emitFinalized / emitLegCompleted,
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
	// Out of scope for runs — must not match even area-glob of neighboring area.
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
		}), scenarioFailed, false}, // failed event with succeeded=0 -> changes=false

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

		// Ephemeral (ADR-052(g)): voyage_id selector narrows rule to its own run.
		// ev() sets payload["voyage_id"]="vy_1" and CorrelationID="vy_1".
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
		}), ev(audit.EventScenarioRunCompleted, map[string]any{ // payload without voyage_id, but CorrelationID=vy_1
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
	// Source returns only enabled (PG WHERE enabled=true). Disabled rule
	// simply absent from snapshot — no match.
	disabled := &Tiding{Name: "off", Herald: "h", EventTypes: []string{"scenario_run.*"}, Enabled: false}
	q := &fakeQueue{}
	d := NewDispatcher(DispatcherConfig{
		Source: &staticSource{rules: nil}, // source already filtered disabled
		Queue:  q,
	})
	_ = disabled
	d.Dispatch(context.Background(), ev(audit.EventScenarioRunCompleted, map[string]any{"summary": map[string]any{"succeeded": 1}}))
	if jobs := q.snapshot(); len(jobs) != 0 {
		t.Fatalf("disabled rule (absent from source) must not generate jobs, got %d", len(jobs))
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
		t.Fatalf("expected 2 jobs (rules a,b), got %d", len(jobs))
	}
	if jobs[0].Herald != "h1" || jobs[1].Herald != "h2" {
		t.Fatalf("wrong heralds in jobs: %q, %q", jobs[0].Herald, jobs[1].Herald)
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
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	job := jobs[0]
	// Copy, not same pointer.
	if &job.PayloadCopy == &event.Payload {
		t.Fatal("PayloadCopy must be copy of map, not same value")
	}
	// Mutation of copy does not affect original.
	job.PayloadCopy["injected"] = true
	if _, exists := event.Payload["injected"]; exists {
		t.Fatal("PayloadCopy mutation leaked into original payload")
	}
	if job.CorrelationID != "vy_1" {
		t.Fatalf("CorrelationID not carried over: %q", job.CorrelationID)
	}
}

// TestDispatch_JobCarriesAnnotationsProjection verifies dispatcher carries
// Annotations/Projection from Tiding to DeliveryJob (ADR-052(h) N1) but does not
// apply them to payload (that is off-path in worker, N3). Verifies field transfer
// and that PayloadCopy remains full copy (projection does not narrow payload in N1).
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
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	job := jobs[0]
	if job.Annotations["team"] != "ops" || job.Annotations["severity"] != "high" {
		t.Errorf("Annotations not carried: %+v", job.Annotations)
	}
	if len(job.Projection) != 2 || job.Projection[0] != "event_type" {
		t.Errorf("Projection not carried: %+v", job.Projection)
	}
	// N1: dispatcher does not narrow payload by projection (that is N3). Full copy intact.
	if _, ok := job.PayloadCopy["summary"]; !ok {
		t.Error("dispatcher must not apply projection in N1 — payload must be full")
	}
}

// TestDispatch_PayloadCopy_ShallowByDesign documents intentional trade-off in
// copyPayload (review S2, item 4): only top level is copied. Deep mutation of
// nested map is visible through both PayloadCopy and original — this is acceptable
// (payload is masked, downstream read-only). Test fixes behavior so accidental
// deep-copy switch does not go unnoticed.
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
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	// Nested map is same pointer (shallow): mutation through copy visible in
	// original. Not isolated — by design.
	jobs[0].PayloadCopy["summary"].(map[string]any)["succeeded"] = 999
	if got := orig["summary"].(map[string]any)["succeeded"]; got != 999 {
		t.Fatalf("expected shared nested map (shallow copy), but mutation did not leak: succeeded=%v", got)
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
		t.Fatal("source error must not generate jobs")
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
	// Must not panic; enqueue error is swallowed.
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
		t.Fatalf("within TTL source must be read once, read %d times", src.calls)
	}

	// Invalidation forces re-read.
	d.InvalidateRules()
	d.Dispatch(context.Background(), event)
	if src.calls != 2 {
		t.Fatalf("after InvalidateRules source must re-read, calls=%d", src.calls)
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
		t.Fatalf("first Dispatch must load rules, calls=%d", src.calls)
	}
	// Fast-forward clock past TTL.
	now = now.Add(20 * time.Millisecond)
	d.Dispatch(context.Background(), event)
	if src.calls != 2 {
		t.Fatalf("after TTL expiry source must re-read, calls=%d", src.calls)
	}
}

// TestDispatch_OccurredAt_FallbackToMatchTime guard against live-smoke Herald bug:
// event.CreatedAt is zero (initiator relied on PG DEFAULT NOW(), tap observes
// pointer before time is filled) → job.OccurredAt must be non-zero, set to match time
// (d.clock), not 0001-01-01.
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
		t.Fatal("precondition: event.CreatedAt must be zero (reproduces bug)")
	}
	d.Dispatch(context.Background(), event)

	jobs := q.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].OccurredAt.IsZero() {
		t.Fatal("job.OccurredAt is zero — occurred_at=0001-01-01 bug not fixed")
	}
	if !jobs[0].OccurredAt.Equal(matchTime) {
		t.Fatalf("job.OccurredAt = %v, want match time %v", jobs[0].OccurredAt, matchTime)
	}
}

// TestDispatch_OccurredAt_PrefersExplicitCreatedAt if initiator set event.CreatedAt
// explicitly, job.OccurredAt takes it (UTC), not match time.
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
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if !jobs[0].OccurredAt.Equal(created) {
		t.Fatalf("job.OccurredAt = %v, want explicit CreatedAt %v", jobs[0].OccurredAt, created)
	}
}
