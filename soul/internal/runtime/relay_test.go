package runtime

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// streamSink is a stream: it records what it carried and breaks after failAfter
// messages when failAfter > 0.
type streamSink struct {
	mu        sync.Mutex
	got       []string
	failAfter int
}

func (s *streamSink) take(label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfter > 0 && len(s.got) >= s.failAfter {
		return errors.New("stream broken")
	}
	s.got = append(s.got, label)
	return nil
}

func (s *streamSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	return s.take(fmt.Sprintf("%s/ev%d", ev.GetApplyId(), ev.GetTaskIdx()))
}

func (s *streamSink) SendRunResult(rr *keeperv1.RunResult) error {
	return s.take(rr.GetApplyId() + "/result")
}

func (s *streamSink) carried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func taskEv(applyID string, idx int32) *keeperv1.TaskEvent {
	return &keeperv1.TaskEvent{ApplyId: applyID, TaskIdx: idx}
}

func owned(applyID string, passage int32) OwnedApply {
	return OwnedApply{ApplyID: applyID, Passage: passage}
}

// ★ GUARD (NIM-949, acceptance 3, Soul side). What an apply reports while no
// stream is attached reaches the next stream, in order, and a Send never fails
// the run; meanwhile the message is declared by apply and passage.
//
// Mutation: drop events while detached → the second stream carries nothing → red.
func TestRelay_QueuesWhileDetachedAndFlushesOnAttach(t *testing.T) {
	r := NewRelay()
	if err := r.SendTaskEvent(&keeperv1.TaskEvent{ApplyId: "a1", Passage: 2}); err != nil {
		t.Fatalf("SendTaskEvent with no stream = %v, want nil", err)
	}
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "a1", Passage: 2})
	if got := r.Undelivered(); !reflect.DeepEqual(got, []OwnedApply{owned("a1", 2)}) {
		t.Errorf("Undelivered = %v, want [a1/2]", got)
	}

	s := &streamSink{}
	r.Attach(s)
	if got, want := s.carried(), []string{"a1/ev0", "a1/result"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stream carried %v, want %v", got, want)
	}
}

// ★ GUARD (NIM-949). A stream that breaks mid-apply loses what it was given in
// flight, so the next stream gets everything not confirmed — the events the dead
// stream accepted, again, then the rest.
//
// Mutation: replay only never-sent entries → the new stream misses a1/ev0 → red.
func TestRelay_ReplaysWhatADeadStreamMayHaveLost(t *testing.T) {
	r := NewRelay()
	dead := &streamSink{failAfter: 1}
	r.Attach(dead)
	_ = r.SendTaskEvent(taskEv("a1", 0))
	_ = r.SendTaskEvent(taskEv("a1", 1))
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "a1"})
	if got, want := dead.carried(), []string{"a1/ev0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dead stream carried %v, want %v", got, want)
	}

	next := &streamSink{}
	r.Attach(next)
	if got, want := next.carried(), []string{"a1/ev0", "a1/ev1", "a1/result"}; !reflect.DeepEqual(got, want) {
		t.Errorf("next stream carried %v, want %v", got, want)
	}
}

// ★ GUARD (NIM-949). A stream that stayed up relayConfirmAfter past an event
// delivered it, and the relay knows it when the stream ends — the order
// production takes: the session detaches, and only then does the next stream's
// WardRoster ask what is undelivered. A delivered message is then neither
// declared nor replayed.
//
// Mutation: prune only while a stream is attached → the WardRoster still
// declares a1 and the next stream gets it again → red.
func TestRelay_DeliveredIsForgottenWhenTheStreamEnds(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r := NewRelay()
	r.nowFn = func() time.Time { return now }
	first := &streamSink{}
	r.Attach(first)
	_ = r.SendTaskEvent(taskEv("a1", 0))
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "a1"})

	now = now.Add(relayConfirmAfter)
	r.Detach(first)
	if got := r.Undelivered(); len(got) != 0 {
		t.Errorf("Undelivered after a long-lived stream ended = %v, want none", got)
	}
	next := &streamSink{}
	r.Attach(next)
	if got := next.carried(); len(got) != 0 {
		t.Errorf("next stream carried %v, want nothing (delivered on the first)", got)
	}
}

func wireSizeOf(ev *keeperv1.TaskEvent) int {
	return (&relayEntry{ev: ev}).wireSize()
}

// ★ GUARD (NIM-949). A TaskEvent the Keeper refuses kills every stream it is sent
// on; the relay gives up the event of the size the refusal quotes after
// relayMaxRefusals refusals instead of holding the Soul in a reconnect loop. The
// events sent with it stay, the RunResult among them, or the step would wait out
// scenario_timeout. A refusal learned after a failed Send detached the stream
// counts as well.
//
// Mutations: no cap → the last stream is handed ev0 again → red; strike every
// event the stream carried → ev1 given up too → red; Refused ignores a stream
// already detached → ev0 is refused only twice and replayed → red.
func TestRelay_GivesUpAnEventTheKeeperRefuses(t *testing.T) {
	r := NewRelay()
	poison := taskEv("a1", 0)
	_ = r.SendTaskEvent(poison)
	_ = r.SendTaskEvent(taskEv("a1", 1))
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "a1"})
	for i := 0; i < relayMaxRefusals; i++ {
		s := &streamSink{}
		if i == 0 {
			// Carries both events and breaks on the RunResult, so the relay has
			// detached itself by the time the refusal is read.
			s.failAfter = 2
		}
		r.Attach(s)
		if got := s.carried(); len(got) < 2 || got[0] != "a1/ev0" || got[1] != "a1/ev1" {
			t.Fatalf("stream %d carried %v, want both events first", i+1, got)
		}
		r.Refused(s, wireSizeOf(poison))
	}
	last := &streamSink{}
	r.Attach(last)
	if got, want := last.carried(), []string{"a1/ev1", "a1/result"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stream %d carried %v, want %v (ev0 given up)", relayMaxRefusals+1, got, want)
	}
}

// ★ GUARD (NIM-949). A RunResult is never given up, whatever the refusal quotes.
//
// Mutation: drop the TaskEvent-only condition → the RunResult is given up → red.
func TestRelay_NeverGivesUpARunResult(t *testing.T) {
	r := NewRelay()
	rr := &keeperv1.RunResult{ApplyId: "a1"}
	_ = r.SendRunResult(rr)
	for range relayMaxRefusals {
		s := &streamSink{}
		r.Attach(s)
		r.Refused(s, (&relayEntry{rr: rr}).wireSize())
	}
	last := &streamSink{}
	r.Attach(last)
	if got := last.carried(); !reflect.DeepEqual(got, []string{"a1/result"}) {
		t.Errorf("stream carried %v, want the RunResult", got)
	}
}

// marshalingSink breaks, as a gRPC stream does, on a message it cannot serialize.
type marshalingSink struct{ streamSink }

func (s *marshalingSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	if _, err := proto.Marshal(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: ev}}); err != nil {
		s.mu.Lock()
		s.failAfter = len(s.got) + 1
		s.got = append(s.got, "broken")
		s.mu.Unlock()
		return err
	}
	return s.streamSink.SendTaskEvent(ev)
}

// ★ GUARD (NIM-949). An event no stream can serialize is dropped at the first try:
// replayed, it would end every stream the Soul opens.
//
// Mutation: drop the marshal check → the next stream breaks on it again → red.
func TestRelay_DropsAnEventThatCannotBeSerialized(t *testing.T) {
	r := NewRelay()
	_ = r.SendTaskEvent(&keeperv1.TaskEvent{ApplyId: "a1", Error: &keeperv1.TaskError{Message: "\xff"}})
	_ = r.SendTaskEvent(taskEv("a1", 1))
	r.Attach(&marshalingSink{})

	next := &marshalingSink{}
	r.Attach(next)
	if got := next.carried(); !reflect.DeepEqual(got, []string{"a1/ev1"}) {
		t.Errorf("next stream carried %v, want only ev1", got)
	}
}

// ★ GUARD (NIM-949). Streams that end without a refusal — a Keeper restarting,
// one instance after another — give nothing up, however many there are.
//
// Mutation: count a refusal on every Detach → the event is dropped → red.
func TestRelay_ARestartingKeeperGivesNothingUp(t *testing.T) {
	r := NewRelay()
	_ = r.SendTaskEvent(taskEv("a1", 0))
	for i := 0; i < 2*relayMaxRefusals; i++ {
		s := &streamSink{}
		r.Attach(s)
		r.Detach(s)
	}
	last := &streamSink{}
	r.Attach(last)
	if got := last.carried(); !reflect.DeepEqual(got, []string{"a1/ev0"}) {
		t.Errorf("stream after %d restarts carried %v, want the event", 2*relayMaxRefusals, got)
	}
}

// ★ GUARD (NIM-949). A later message of an apply settles its earlier ones; another
// apply's events — a host serves several runs — stay until delivered.
//
// Mutations: Settle drops every finished apply → b1 lost → red; Settle keeps the
// earlier passage → a1/0 still declared → red.
func TestRelay_SettleIsPerApply(t *testing.T) {
	r := NewRelay()
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "a1", Passage: 0})
	_ = r.SendRunResult(&keeperv1.RunResult{ApplyId: "b1", Passage: 0})
	r.Settle("a1", 1)
	if got := r.Undelivered(); !reflect.DeepEqual(got, []OwnedApply{owned("b1", 0)}) {
		t.Errorf("Undelivered after a1's next message = %v, want [b1/0]", got)
	}
}

// ★ GUARD (NIM-949). The WardRoster names each message the Soul still answers
// for outside Run, by passage — or the Keeper's sweep orphans a step whose result
// is queued, or spares a later step the Soul never received.
//
// Mutations: ActiveSet ignores alsoOwned → a9 missing → red; drop the passage →
// it reads 0 → red.
func TestActiveSet_IncludesAlsoOwned(t *testing.T) {
	r := NewApplyRunner(nil, nil)
	r.SetAlsoOwned(func() []OwnedApply { return []OwnedApply{owned("a9", 4)} })
	got := r.ActiveSet()
	if len(got) != 1 || got[0].GetApplyId() != "a9" || got[0].Passage == nil || got[0].GetPassage() != 4 {
		t.Errorf("ActiveSet = %v, want [a9 passage 4]", got)
	}
}
