package soul_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// pollCounter counts the calls a fake has answered. Every test below needs a
// second poll to happen inside the wait; asserting it turns a run starved by the
// scheduler into a visible failure instead of a vacuous pass.
type pollCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *pollCounter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.calls
}

func (c *pollCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// hangUntilDone blocks until the caller's context ends and fails with its error,
// wrapped the way keeperredis.SoulsStreamAlive wraps it.
func hangUntilDone(ctx context.Context, what string) error {
	<-ctx.Done()
	return fmt.Errorf("%s: pipeline EXEC: %w", what, ctx.Err())
}

// deadlinePresence answers its first `answered` polls and hangs every later one
// until the wait ends. answered=1 is NIM-886's live failure: the last poll of a
// 15-minute barrier cut by the barrier's own deadline. answered=0 is a presence
// source that never answers at all.
type deadlinePresence struct {
	pollCounter
	answered int
}

func (p *deadlinePresence) SoulsStreamAlive(ctx context.Context, _ []string) (map[string]struct{}, error) {
	if p.next() <= p.answered {
		return map[string]struct{}{}, nil
	}
	return nil, hangUntilDone(ctx, "redis.SoulsStreamAlive")
}

// alwaysOnline reports every SID online, so the facts poll is the one that runs.
type alwaysOnline struct{}

func (alwaysOnline) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(sids))
	for _, sid := range sids {
		out[sid] = struct{}{}
	}
	return out, nil
}

// deadlineFactsStore is the same cut on the other poll the barrier makes: the
// soulprint check behind `refresh_soulprint: true`.
type deadlineFactsStore struct {
	*fakeStore
	pollCounter
}

func (s *deadlineFactsStore) SoulsWithSoulprint(ctx context.Context, _ []string) (map[string]struct{}, error) {
	if s.next() == 1 {
		return map[string]struct{}{}, nil
	}
	return nil, hangUntilDone(ctx, "soul.SoulsWithSoulprint")
}

// ctxHonouringStore refuses a read on a context that is already done, as the
// Postgres store does. The plain fake answers regardless, which would hide a
// lookup made on the run's own context after the run ended.
type ctxHonouringStore struct{ *fakeStore }

func (s *ctxHonouringStore) SelectBySID(ctx context.Context, sid string) (*keepersoul.Soul, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.fakeStore.SelectBySID(ctx, sid)
}

// failingAfterFirst answers its first poll and then fails every later one at once,
// with the barrier's context still alive — a presence source that really is down.
type failingAfterFirst struct{ pollCounter }

func (p *failingAfterFirst) SoulsStreamAlive(context.Context, []string) (map[string]struct{}, error) {
	if p.next() == 1 {
		return map[string]struct{}{}, nil
	}
	return nil, errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
}

// hookedPresence reports nobody online and calls onPoll with the 1-based poll
// number, letting a test act while the barrier is waiting.
type hookedPresence struct {
	pollCounter
	onPoll func(n int)
}

func (p *hookedPresence) SoulsStreamAlive(context.Context, []string) (map[string]struct{}, error) {
	n := p.next()
	if p.onPoll != nil {
		p.onPoll(n)
	}
	return map[string]struct{}{}, nil
}

func barrierParams(sid string, refresh bool) map[string]any {
	return map[string]any{
		"sid":                 sid,
		"await_online":        true,
		"await_timeout":       "500ms",
		"await_poll_interval": "1ms",
		"refresh_soulprint":   refresh,
	}
}

func runBarrier(t *testing.T, m *coremodsoul.Module, stream *internaltest.ApplyStream, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "registered", Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || !ev.Failed {
		t.Fatalf("expected the barrier to fail, got %+v", ev)
	}
	return ev
}

// ★ NIM-886. A barrier that times out must not blame the source it polls for its
// own deadline. Live, the step ended "(last error: redis.SoulsStreamAlive:
// pipeline EXEC: context deadline exceeded)" over a Redis that was healthy, and
// the operator spent the time after the 15-minute wait on the wrong subsystem.
// Both polls are cut here — presence, and the soulprint check of
// `refresh_soulprint: true` — because each records its own error. And the
// message must still name the shortfall from the last complete poll: a cut poll
// that wiped it would leave a red step that says nothing.
func TestAwait_OwnDeadlineIsNotReportedAsTheCause(t *testing.T) {
	type setup struct {
		store    coremodsoul.Store
		presence coremodsoul.PresenceChecker
		polls    func() int
	}
	cases := []struct {
		name      string
		refresh   bool
		build     func() setup
		shortfall string
	}{
		{
			name: "presence poll cut, presence-only barrier",
			build: func() setup {
				p := &deadlinePresence{answered: 1}
				return setup{newFakeStore(), p, p.count}
			},
			shortfall: "(pending: [h1.example.com])",
		},
		{
			name:    "presence poll cut, soulprint barrier",
			refresh: true,
			build: func() setup {
				p := &deadlinePresence{answered: 1}
				return setup{newFakeStore(), p, p.count}
			},
			shortfall: "(not online: [h1.example.com])",
		},
		{
			name:    "soulprint poll cut",
			refresh: true,
			build: func() setup {
				s := &deadlineFactsStore{fakeStore: newFakeStore()}
				return setup{s, alwaysOnline{}, s.count}
			},
			shortfall: "(online but factless: [h1.example.com])",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build()
			m := newAwaitModule(t, s.store, s.presence, "")
			ev := runBarrier(t, m, internaltest.NewApplyStream(), barrierParams("h1.example.com", tc.refresh))
			if s.polls() < 2 {
				t.Fatalf("only %d poll(s) happened — the cut this test is about never ran", s.polls())
			}
			if strings.Contains(ev.Message, "deadline exceeded") || strings.Contains(ev.Message, "last error") ||
				strings.Contains(ev.Message, "last presence error") {
				t.Errorf("timeout message blames a poll the barrier's own deadline cut:\n%s", ev.Message)
			}
			if !strings.Contains(ev.Message, tc.shortfall) {
				t.Errorf("timeout message lost the last complete answer %q:\n%s", tc.shortfall, ev.Message)
			}
		})
	}
}

// The other side of the same line: a presence source that is really failing,
// with the barrier's context still alive, is still named. Without this the fix
// above could be "never report a poll error", which hides a down Redis behind
// "hosts not onboarded" — the confusion `last error` exists to prevent.
func TestAwait_RealPresenceFailureIsStillReported(t *testing.T) {
	p := &failingAfterFirst{}
	m := newAwaitModule(t, newFakeStore(), p, "")
	ev := runBarrier(t, m, internaltest.NewApplyStream(), barrierParams("h1.example.com", false))
	if p.count() < 2 {
		t.Fatalf("only %d poll(s) happened — the failing one never ran", p.count())
	}
	if !strings.Contains(ev.Message, "connection refused") {
		t.Errorf("a presence source that is down must be named in the timeout message:\n%s", ev.Message)
	}
}

// A presence source that never answers during the whole wait is that source's
// failure, deadline or not. Dropping every error cut by the wait would turn an
// unreachable Redis — a dial that outlasts `await_timeout` — into "0/1 online",
// blaming the hosts.
func TestAwait_PresenceThatNeverAnsweredIsStillNamed(t *testing.T) {
	m := newAwaitModule(t, newFakeStore(), &deadlinePresence{answered: 0}, "")
	ev := runBarrier(t, m, internaltest.NewApplyStream(), barrierParams("h1.example.com", false))
	if !strings.Contains(ev.Message, "presence check failed") || !strings.Contains(ev.Message, "redis.SoulsStreamAlive") {
		t.Errorf("a presence source that never answered must be named as the failure:\n%s", ev.Message)
	}
}

// A run that ends while the barrier waits is reported as that, not as a barrier
// timeout "within 500ms" that never elapsed — and it still names the hosts it
// was waiting for. The run's deadline can be the shorter one — a plan without
// `refresh_soulprint` keeps the 5-minute default — and a message reduced to the
// bare context error would be NIM-886's false trail again, with no host in it.
func TestAwait_RunEndingIsNotReportedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const sid = "h1.example.com"
	fs := newFakeStore()
	fs.byID[sid] = &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
	p := &hookedPresence{onPoll: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	m := newAwaitModule(t, &ctxHonouringStore{fs}, p, "")
	ev := runBarrier(t, m, internaltest.NewApplyStreamCtx(ctx), barrierParams(sid, false))
	for _, want := range []string{
		"before the run ended (context canceled)",
		"(pending: [" + sid + "])",
		sid + ": no stream on record",
	} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("a run ended mid-wait must say so and still name the hosts — lacks %q:\n%s", want, ev.Message)
		}
	}
	if strings.Contains(ev.Message, "within") {
		t.Errorf("a cancelled run reported as a barrier timeout:\n%s", ev.Message)
	}
}

// A source that answered once and then hung through the end of the wait is
// that source's failure, not a poll the deadline merely caught in flight. The
// facts check is the one with no timeout of its own (pgx sets none), so a hung
// Postgres would otherwise leave the hosts blamed for the shortfall.
func TestAwait_SourceHungThroughTheDeadlineIsNamed(t *testing.T) {
	s := &deadlineFactsStore{fakeStore: newFakeStore()}
	m := newAwaitModule(t, s, alwaysOnline{}, "")
	params := barrierParams("h1.example.com", true)
	params["await_timeout"] = "1500ms"
	ev := runBarrier(t, m, internaltest.NewApplyStream(), params)
	if s.count() < 2 {
		t.Fatalf("only %d facts poll(s) happened — the hanging one never ran", s.count())
	}
	if !strings.Contains(ev.Message, "soul.SoulsWithSoulprint") || !strings.Contains(ev.Message, "no answer for") {
		t.Errorf("a facts check that hung through the deadline must be named:\n%s", ev.Message)
	}
}

// failingThenHanging fails its first poll for a real reason and hangs every
// later one until the wait ends.
type failingThenHanging struct{ pollCounter }

func (p *failingThenHanging) SoulsStreamAlive(ctx context.Context, _ []string) (map[string]struct{}, error) {
	if p.next() == 1 {
		return nil, errors.New("dial tcp 10.0.0.1:6379: connect: no route to host")
	}
	return nil, hangUntilDone(ctx, "redis.SoulsStreamAlive")
}

// The root cause of a presence source that never answered is the real error it
// gave, not the deadline text of the poll the wait cut last — even when that
// poll hung past the hang threshold. The 1.5s wait makes it hang that long.
func TestAwait_FirstRealPresenceErrorIsKept(t *testing.T) {
	p := &failingThenHanging{}
	m := newAwaitModule(t, newFakeStore(), p, "")
	params := barrierParams("h1.example.com", false)
	params["await_timeout"] = "1500ms"
	ev := runBarrier(t, m, internaltest.NewApplyStream(), params)
	if p.count() < 2 {
		t.Fatalf("only %d poll(s) happened — the cut one never ran", p.count())
	}
	if !strings.Contains(ev.Message, "presence check failed") || !strings.Contains(ev.Message, "no route to host") {
		t.Errorf("the first real presence error must be the one named:\n%s", ev.Message)
	}
	if strings.Contains(ev.Message, "deadline exceeded") {
		t.Errorf("the deadline of a cut poll replaced the real cause:\n%s", ev.Message)
	}
}

// failingThenOnline fails its first poll for a real reason and then reports
// every SID online.
type failingThenOnline struct{ pollCounter }

func (p *failingThenOnline) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	if p.next() == 1 {
		return nil, errors.New("dial tcp 10.0.0.1:6379: connect: connection refused")
	}
	return alwaysOnline{}.SoulsStreamAlive(context.Background(), sids)
}

// hangingFactsStore never answers a facts poll until the wait ends.
type hangingFactsStore struct {
	*fakeStore
	pollCounter
}

func (s *hangingFactsStore) SoulsWithSoulprint(ctx context.Context, _ []string) (map[string]struct{}, error) {
	s.next()
	return nil, hangUntilDone(ctx, "soul.SoulsWithSoulprint")
}

// Presence that recovers is not turned into a fatal presence failure because the
// facts poll riding on its answer was cut by the end of the wait: the excused
// facts poll keeps the presence half when there is no earlier answer to keep,
// and the real error Redis gave before it recovered is still the one reported.
func TestAwait_PresenceAnswerSurvivesAnExcusedFactsPoll(t *testing.T) {
	p := &failingThenOnline{}
	s := &hangingFactsStore{fakeStore: newFakeStore()}
	m := newAwaitModule(t, s, p, "")
	ev := runBarrier(t, m, internaltest.NewApplyStream(), barrierParams("h1.example.com", true))
	if p.count() < 2 || s.count() < 1 {
		t.Fatalf("polls: presence %d, facts %d — the recovered presence poll never ran", p.count(), s.count())
	}
	if strings.Contains(ev.Message, "presence check failed") {
		t.Errorf("presence answered, yet the step reports a presence failure:\n%s", ev.Message)
	}
	if !strings.Contains(ev.Message, "0/1 souls ready") || strings.Contains(ev.Message, "not online") {
		t.Errorf("the recovered presence answer was not kept:\n%s", ev.Message)
	}
	if !strings.Contains(ev.Message, "(last error: dial tcp 10.0.0.1:6379: connect: connection refused)") {
		t.Errorf("the held real error was dropped when the presence half was kept:\n%s", ev.Message)
	}
}

// ★ NIM-886. A timeout names, per host the barrier gave up on, what the registry
// holds about its stream. The three answers point at different faults: no stream
// on record (nothing ever connected under the current registration), a last
// stream from before the wait (a stopped agent, or a record whose machine may no
// longer hold the identity), and one during the wait (the agent itself).
func TestAwait_TimeoutNamesWhatTheRegistryKnowsPerHost(t *testing.T) {
	const never, stale, dropped = "never.example.com", "stale.example.com", "dropped.example.com"
	staleAt := time.Date(2026, 9, 12, 10, 1, 22, 0, time.UTC)

	fs := newFakeStore()
	fs.byID[never] = &keepersoul.Soul{SID: never, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
	fs.byID[stale] = &keepersoul.Soul{SID: stale, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusDisconnected, LastSeenAt: &staleAt}
	fs.byID[dropped] = &keepersoul.Soul{SID: dropped, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}

	// `dropped` streams during the wait — its last_seen_at is flushed between two
	// polls, as the EventStream does on first contact — and is gone by the end.
	// Stamped from inside the wait, so the line is drawn at the wait's START: a
	// classifier comparing against the time of the message would call it stale.
	p := &hookedPresence{onPoll: func(n int) {
		if n == 2 {
			at := time.Now()
			fs.byID[dropped].LastSeenAt = &at
		}
	}}
	m := newAwaitModule(t, fs, p, "")
	params := barrierParams(never, true)
	params["sid"] = []any{never, stale, dropped}
	ev := runBarrier(t, m, internaltest.NewApplyStream(), params)
	if p.count() < 2 {
		t.Fatalf("only %d poll(s) happened — %s never streamed during the wait", p.count(), dropped)
	}
	for _, want := range []string{
		never + ": no stream on record",
		stale + ": last stream on record 2026-09-12T10:01:22Z, before the wait began",
		dropped + ": last stream on record ",
	} {
		if !strings.Contains(ev.Message, want) {
			t.Errorf("timeout message lacks %q:\n%s", want, ev.Message)
		}
	}
	if i := strings.Index(ev.Message, dropped+": "); i < 0 || !strings.Contains(ev.Message[i:], ", during the wait") {
		t.Errorf("%s streamed during the wait and is not said to have:\n%s", dropped, ev.Message)
	}
}
