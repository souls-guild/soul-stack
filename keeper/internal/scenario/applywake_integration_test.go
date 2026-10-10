//go:build integration

package scenario

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applywake"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/cel"
)

const threeIndependentTasks = `name: create
description: three tasks, none reading another's register
tasks:
  - name: first
    module: core.exec.run
    changed_when: "false"
    params: { cmd: first }
  - name: second
    module: core.exec.run
    changed_when: "false"
    params: { cmd: second }
  - name: third
    module: core.exec.run
    changed_when: "false"
    params: { cmd: third }
`

// answerAfter is how long a host takes to answer: past the barrier's first read,
// so only a wake or a poll can end the wait.
const answerAfter = 50 * time.Millisecond

// wakingDispatcher answers like a Soul whose RunResult lands on the Keeper instance
// owning writer: after answerAfter it writes the host's terminal and, unless silent,
// wakes the run's barrier through that instance's hub.
type wakingDispatcher struct {
	t      *testing.T
	writer *applywake.Hub
	silent bool
}

func (d *wakingDispatcher) SendApply(_ context.Context, sid string, req *keeperv1.ApplyRequest) error {
	applyID, passage := req.GetApplyId(), int(req.GetPassage())
	go func() {
		time.Sleep(answerAfter)
		if err := applyrun.UpdateStatus(context.Background(), integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
			d.t.Errorf("UpdateStatus(%s, passage=%d): %v", sid, passage, err)
			return
		}
		if !d.silent {
			d.writer.Wake(applyID)
		}
	}()
	return nil
}

func newWakeRunner(t *testing.T, disp ApplyDispatcher, hub *applywake.Hub, fallback time.Duration) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:          artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:        topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:     servicevars.NewResolver(nil),
		Render:          render.NewPipeline(nil, engine, nil, nil),
		Outbound:        disp,
		DB:              integrationPool,
		Audit:           auditpg.NewWriter(integrationPool),
		AuditReader:     auditpg.NewReader(integrationPool),
		SoulCap:         stubSoulCap{},
		PollInterval:    20 * time.Millisecond,
		RunTimeout:      20 * time.Second,
		ApplyWake:       hub,
		BarrierFallback: fallback,
	})
}

func seedWakeRun(t *testing.T) string {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	return writeServiceRepo(t, threeIndependentTasks)
}

// ★ GUARD (NIM-984). A barrier ends on the write of the last terminal it waits for,
// not on its next poll: with the fallback poll at 10 s, three linear steps — three
// barriers — and one staged barrier all end within 5 s, whether the terminal is
// written on the run's own instance or on another one through Redis. T17 (NIM-950)
// measured the 200 ms poll as ~95% of the gap between two linear steps.
//
// Mutations: drop `case <-wake` from waitStep → the linear run waits a fallback poll
// per step; from waitBarrierExcept → the staged run waits one.
func TestIntegration_BarrierEndsOnTheWakeNotOnTheFallbackPoll(t *testing.T) {
	const fallback = 10 * time.Second
	const within = 5 * time.Second

	mr := miniredis.RunT(t)
	logger := slog.New(slog.DiscardHandler)
	client := func() *keeperredis.Client {
		c, err := keeperredis.NewClient(context.Background(), keeperredis.Config{Addr: mr.Addr()}, nil)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := applywake.New(client(), "kid-owner", logger)
	other := applywake.New(client(), "kid-other", logger)
	go owner.Listen(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for mr.PubSubNumSub(keeperredis.ApplyWakeChannel)[keeperredis.ApplyWakeChannel] < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the run's instance never subscribed to barrier wakes")
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, tc := range []struct {
		name   string
		model  ExecutionModel
		writer *applywake.Hub
	}{
		{"linear, terminal written here", ExecutionLinear, owner},
		{"linear, terminal written on another instance", ExecutionLinear, other},
		{"staged, terminal written here", ExecutionStaged, owner},
		{"staged, terminal written on another instance", ExecutionStaged, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitURL := seedWakeRun(t)
			r := newWakeRunner(t, &wakingDispatcher{t: t, writer: tc.writer}, owner, fallback)
			start := time.Now()
			applyID := startModelRun(t, r, tc.model, "noop-prod", "noop", gitURL, "create")
			waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
			if took := time.Since(start); took > within {
				t.Fatalf("the run took %s with every terminal announced; want under %s — its barrier waited for the %s fallback poll", took, within, fallback)
			}
			wantSteps := 1
			if tc.model == ExecutionLinear {
				wantSteps = 3
			}
			if rows := hostRows(t, applyID); len(rows) != wantSteps {
				t.Fatalf("host rows = %d, want %d: the run is not the shape this guard times", len(rows), wantSteps)
			}
		})
	}
}

// ★ GUARD (NIM-984). A wake is a hint: one that never comes — a Redis outage, a
// writer that does not announce — leaves the barrier to its fallback poll, and the
// run still ends.
//
// Mutation: drop `case <-ticker.C` from waitStep → the run never ends.
func TestIntegration_BarrierWithoutAWakeEndsAtTheFallbackPoll(t *testing.T) {
	gitURL := seedWakeRun(t)
	hub := applywake.New(nil, "", nil)
	r := newWakeRunner(t, &wakingDispatcher{t: t, writer: hub, silent: true}, hub, 300*time.Millisecond)
	applyID := startModelRun(t, r, ExecutionLinear, "noop-prod", "noop", gitURL, "create")
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
}
