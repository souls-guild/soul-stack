//go:build integration

package scenario

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serialBlock takes the block's settings: line, or nothing.
const serialBlock = `name: create
tasks:
  - name: Upgrade then check
    serial: 1
%s    block:
      - name: Upgrade
        module: core.exec.run
        params: { cmd: echo, args: ["upgrade"] }
        changed_when: "false"
      - name: Check
        module: core.exec.run
        params: { cmd: echo, args: ["check"] }
        changed_when: "false"
`

const serialTaskThenPlain = `name: create
tasks:
  - name: Upgrade
    module: core.exec.run
    serial: 1
    params: { cmd: echo, args: ["upgrade"] }
    changed_when: "false"
  - name: Check
    module: core.exec.run
    params: { cmd: echo, args: ["check"] }
    changed_when: "false"
`

// rollOrderDispatcher records each message as "<task>+<task>@<host>", in send
// order.
type rollOrderDispatcher struct {
	t    *testing.T
	mu   sync.Mutex
	sent []string
}

func (d *rollOrderDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	var names []string
	for _, task := range req.GetTasks() {
		names = append(names, task.GetName())
	}
	d.mu.Lock()
	d.sent = append(d.sent, strings.Join(names, "+")+"@"+strings.TrimSuffix(sid, ".example.com"))
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, int(req.GetPassage()), applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("rollOrderDispatcher: UpdateStatus(%s): %v", sid, err)
	}
	return nil
}

func (d *rollOrderDispatcher) messages() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.sent...)
}

func startSerialScopeRun(t *testing.T, gitURL string, disp ApplyDispatcher) string {
	t.Helper()
	r := newRunner(t, disp, gitURL)
	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return applyID
}

// ★ GUARD (NIM-998, settings.md §3.1). The Keeper accepts a serial: block that
// writes no serial_scope and rolls it by task: Upgrade reaches every host before
// Check reaches any. serial_scope: block is the canary: Upgrade and Check reach one
// host in one message before the next host is touched. A serial: task beside a
// plain host step is accepted too, and the plain step follows it on every host. The
// send order cannot tell the waves apart; the widths are
// TestRender_LinearSerialByBlockMode's.
//
// Mutations: default serial_scope to block in DefaultScenarioSettings → the
// unwritten block arrives as the canary; put back the NIM-940 refusal on the run
// path → the run error-locks before anything is sent.
func TestIntegration_SerialScope_DefaultRollsByTask(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		inOrder        []string
		// lastWave follows inOrder as one wave, in any order.
		lastWave []string
	}{
		{
			name:     "a block without serial_scope",
			scenario: fmt.Sprintf(serialBlock, ""),
			inOrder:  []string{"Upgrade@host-a", "Upgrade@host-b", "Upgrade@host-c", "Check@host-a", "Check@host-b", "Check@host-c"},
		},
		{
			name:     "serial_scope: block is the canary",
			scenario: fmt.Sprintf(serialBlock, "    settings: { serial_scope: block }\n"),
			inOrder:  []string{"Upgrade+Check@host-a", "Upgrade+Check@host-b", "Upgrade+Check@host-c"},
		},
		{
			name:     "a serial: task beside a plain host step",
			scenario: serialTaskThenPlain,
			inOrder:  []string{"Upgrade@host-a", "Upgrade@host-b", "Upgrade@host-c"},
			lastWave: []string{"Check@host-a", "Check@host-b", "Check@host-c"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "noop-prod")
			for _, h := range []string{"host-a", "host-b", "host-c"} {
				seedConnectedSoul(t, h+".example.com", []string{"noop-prod"})
			}
			disp := &rollOrderDispatcher{t: t}
			applyID := startSerialScopeRun(t, writeServiceRepo(t, tc.scenario), disp)

			inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
			got := disp.messages()
			if len(got) != len(tc.inOrder)+len(tc.lastWave) {
				t.Fatalf("messages = %v (status details %v), want %v then %v", got, inc.StatusDetails, tc.inOrder, tc.lastWave)
			}
			last := append([]string(nil), got[len(tc.inOrder):]...)
			sort.Strings(last)
			if !reflect.DeepEqual(got[:len(tc.inOrder)], tc.inOrder) || (tc.lastWave != nil && !reflect.DeepEqual(last, tc.lastWave)) {
				t.Errorf("messages = %v, want %v then %v", got, tc.inOrder, tc.lastWave)
			}
		})
	}
}
