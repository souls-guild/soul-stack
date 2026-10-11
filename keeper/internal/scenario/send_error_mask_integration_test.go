//go:build integration

package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// echoingSendDispatcher fails every send with an error echoing the request's
// params, as a transport or marshal error can. The sends come from the step's
// waiting goroutines, hence the lock.
type echoingSendDispatcher struct {
	mu    sync.Mutex
	calls int
	wire  string
}

func (d *echoingSendDispatcher) SendApply(_ context.Context, _ string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if tasks := req.GetTasks(); len(tasks) > 0 {
		d.wire = renderedExecCommand(tasks[0].GetParams())
	}
	return fmt.Errorf("rpc transport: failed to send %s", d.wire)
}

// ★ GUARD (NIM-952), the linear twin of
// TestIntegration_SecretInParams_MaskedInObservability_NotOnWire. A linear run
// does not fail a host whose send fails: it logs the failure and retries until
// soul_timeout. That log line must not carry what the error echoes of the request:
// neither the value of a sealed params cell (a secret input), redacted in place,
// nor a vault reference, which masks the whole error.
//
// Mutations: log the raw send error in stepWatch.trySend → both red; build the
// stepWatch with no sealed snapshot → "sealed input" red.
func TestIntegration_LinearSendError_MaskedInRunLog(t *testing.T) {
	const secret = "s3cr3t-key-91c4"
	for _, tc := range []struct {
		name, arg, wire, leak, kept string
	}{
		{"sealed input", "--key=${ input.key }", "deploy --key=" + secret, secret, "rpc transport: failed to send deploy " + audit.MaskedValue},
		{"vault reference", "--token=vault:secret/keeper/deploy-token", "deploy --token=vault:secret/keeper/deploy-token", "deploy-token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "noop-prod")
			seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
			gitURL := writeServiceRepo(t, `name: create
input:
  key:
    type: string
    secret: true
settings:
  soul_timeout: 500ms
tasks:
  - name: Run with a secret param
    module: core.exec.run
    params:
      cmd: deploy
      args: ["`+tc.arg+`"]
    changed_when: "false"
`)

			disp := &echoingSendDispatcher{}
			r := newRunner(t, disp, gitURL)
			logs := &lockedBuffer{}
			r.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			applyID := audit.NewULID()
			if err := r.Start(context.Background(), RunSpec{
				ApplyID:         applyID,
				IncarnationName: "noop-prod",
				ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
				ScenarioName:    "create",
				Input:           map[string]any{"key": secret},
				StartedByAID:    "archon-alice",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)

			disp.mu.Lock()
			calls, wire := disp.calls, disp.wire
			disp.mu.Unlock()
			if calls == 0 || wire != tc.wire {
				t.Fatalf("sends = %d, wire params = %q, want the unmasked %q on the wire", calls, wire, tc.wire)
			}

			out := logs.String()
			if strings.Contains(out, tc.leak) {
				t.Fatalf("the run log echoes the payload of a failed send:\n%s", out)
			}
			if !strings.Contains(out, "waiting for one") || !strings.Contains(out, tc.kept) {
				t.Fatalf("the failed send was not logged with its masked error %q - the guard reads nothing:\n%s", tc.kept, out)
			}

			st, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
			if err != nil {
				t.Fatalf("SelectStatusesByApplyID: %v", err)
			}
			if len(st) != 1 || st[0].ErrorSummary == nil || !strings.HasPrefix(*st[0].ErrorSummary, soulUnavailable) {
				t.Fatalf("apply_runs = %+v, want one %s row", st, soulUnavailable)
			}
			if strings.Contains(*st[0].ErrorSummary, tc.leak) {
				t.Errorf("error_summary echoes the payload: %q", *st[0].ErrorSummary)
			}
		})
	}
}
