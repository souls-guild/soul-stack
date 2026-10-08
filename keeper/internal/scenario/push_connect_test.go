package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// flakyPush refuses the SSH connect `refusals` times, then runs the host; err, when
// set, is returned instead of a run — a session that broke after the start.
type flakyPush struct {
	refusals int
	err      error
	calls    int
}

func (f *flakyPush) SendApply(_ context.Context, sid string, _ push.Route, _ *keeperv1.ApplyRequest, _ push.EventHandler) (*keeperv1.RunResult, error) {
	f.calls++
	if f.calls <= f.refusals {
		return nil, fmt.Errorf("%w: %s: dial tcp: connection refused", push.ErrConnect, sid)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &keeperv1.RunResult{Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}, nil
}

// ★ GUARD (NIM-949, user decision 2). A push host's soul_timeout is a wait for an
// SSH session: a refused connect is retried until soul_timeout, then the host is
// unavailable; a session that broke after the command started is never retried;
// a staged run keeps failing at once.
//
// Mutations: return the first ErrConnect under linear → "connects late" is red;
// retry every error → "broke mid-run" makes 2 calls → red; drop the deadline →
// "never connects" hangs past the test's own bound → red.
func TestSendPush_RetriesTheConnectUntilSoulTimeout(t *testing.T) {
	short := 150 * time.Millisecond
	linear := RunSpec{executionModel: ExecutionLinear, stepSettings: []config.ScenarioSettings{{SoulTimeout: &short}}}
	broken := errors.New("push: NDJSON stream ended without RunResult")
	log := slog.New(slog.DiscardHandler)

	for _, tc := range []struct {
		name      string
		spec      RunSpec
		push      *flakyPush
		wantErr   error
		wantCalls int
	}{
		{"connects late", linear, &flakyPush{refusals: 2}, nil, 3},
		{"never connects", linear, &flakyPush{refusals: 1 << 30}, errPushUnreachable, -1},
		{"broke mid-run", linear, &flakyPush{err: broken}, broken, 1},
		{"staged fails at once", RunSpec{executionModel: ExecutionStaged}, &flakyPush{refusals: 2}, push.ErrConnect, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{deps: Deps{PushApply: tc.push}, pollInterval: 5 * time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := r.sendPush(ctx, tc.spec, log, 0, "push.example.com", push.Route{}, &keeperv1.ApplyRequest{}, nil)
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if ctx.Err() != nil {
				t.Fatalf("sendPush outlived soul_timeout (%s)", short)
			}
			if tc.wantCalls >= 0 && tc.push.calls != tc.wantCalls {
				t.Errorf("SendApply calls = %d, want %d", tc.push.calls, tc.wantCalls)
			}
		})
	}
}

// ★ GUARD (NIM-949). A push host's connect retries stop when its step ends — a
// failure elsewhere ended it — so the step does not reach a host after it
// failed, as a stream host's wait does not.
//
// Mutation: drop the stepDone case from the retry loop → the loop runs to
// soul_timeout (5s) → red.
func TestSendPush_RetriesStopWithTheStep(t *testing.T) {
	long := 5 * time.Second
	spec := RunSpec{executionModel: ExecutionLinear, stepSettings: []config.ScenarioSettings{{SoulTimeout: &long}}}
	r := &Runner{deps: Deps{PushApply: &flakyPush{refusals: 1 << 30}}, pollInterval: 5 * time.Millisecond}
	w := &stepWatch{done: make(chan struct{})}
	time.AfterFunc(100*time.Millisecond, func() { close(w.done) })

	start := time.Now()
	_, err := r.sendPush(w.pushContext(context.Background()), spec, slog.New(slog.DiscardHandler), 0, "push.example.com", push.Route{}, &keeperv1.ApplyRequest{}, nil)
	if !errors.Is(err, push.ErrConnect) || errors.Is(err, errPushUnreachable) {
		t.Errorf("err = %v, want the last connect error, not soul_unavailable", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("sendPush returned after %s, want soon after the step ended", took)
	}
}
