package herald

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestIsHeraldOwnEvent — классификатор собственных терминалов доставки.
func TestIsHeraldOwnEvent(t *testing.T) {
	cases := []struct {
		et   audit.EventType
		want bool
	}{
		{audit.EventHeraldDelivered, true},
		{audit.EventHeraldFailed, true},
		{audit.EventScenarioRunFailed, false},
		{audit.EventVoyageReclaimed, false},
		{audit.EventCommandRunCompleted, false},
		{audit.EventIncarnationDriftChecked, false},
	}
	for _, c := range cases {
		if got := isHeraldOwnEvent(c.et); got != c.want {
			t.Errorf("isHeraldOwnEvent(%q) = %v, want %v", c.et, got, c.want)
		}
	}
}

// TestDispatch_HeraldOwnEvents_NoLoop — guard-тест на петлю: терминалы доставки
// (`herald.delivered`/`herald.failed`) сами идут через audit-writer → tap →
// Dispatch. Даже при правиле, матчащем их, dispatcher обязан отсечь их ДО матча,
// иначе уведомление об уведомлении зациклится. Конструируем «дырявое» правило с
// herald.*-scope (недопустим на CRUD) и проверяем: ни одного job-а на herald.*.
func TestDispatch_HeraldOwnEvents_NoLoop(t *testing.T) {
	q := &fakeQueue{}
	rule := &Tiding{Name: "evil-loop", Herald: "ch", EventTypes: []string{"herald.*"}, Enabled: true}
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{rules: []*Tiding{rule}}, Queue: q})

	for _, et := range []audit.EventType{audit.EventHeraldDelivered, audit.EventHeraldFailed} {
		d.Dispatch(context.Background(), &audit.Event{EventType: et})
	}

	if jobs := q.snapshot(); len(jobs) != 0 {
		t.Fatalf("herald.* events must never enqueue delivery jobs (loop!), got %d", len(jobs))
	}
}

// TestDispatch_RunEvent_StillMatches — контроль: loop-guard НЕ глушит обычные
// run-события (регресс-страховка, что guard не слишком широк).
func TestDispatch_RunEvent_StillMatches(t *testing.T) {
	q := &fakeQueue{}
	rule := &Tiding{Name: "ok", Herald: "ch", EventTypes: []string{"scenario_run.*"}, Enabled: true}
	d := NewDispatcher(DispatcherConfig{Source: &staticSource{rules: []*Tiding{rule}}, Queue: q})

	d.Dispatch(context.Background(), &audit.Event{EventType: audit.EventScenarioRunFailed})

	if jobs := q.snapshot(); len(jobs) != 1 {
		t.Fatalf("run event must enqueue exactly one job, got %d", len(jobs))
	}
}
