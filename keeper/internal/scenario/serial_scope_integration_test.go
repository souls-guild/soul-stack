//go:build integration

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The Keeper refuses on the run path what soul-lint refuses offline (NIM-940):
// service repositories are not linted, so a refusal only the linter made would
// leave the change silent exactly where it matters.

const serialBlockWithoutScope = `name: create
tasks:
  - name: Roll change and check
    serial: 1
    block:
      - name: Change
        module: core.exec.run
        params: { cmd: echo, args: ["change"] }
        changed_when: "false"
      - name: Check
        module: core.exec.run
        params: { cmd: echo, args: ["check"] }
        changed_when: "false"
`

const serialPairAcrossTasks = `name: create
tasks:
  - name: Change
    module: core.exec.run
    serial: 2
    params: { cmd: echo, args: ["change"] }
    changed_when: "false"
  - name: Check
    module: core.exec.run
    params: { cmd: echo, args: ["check"] }
    changed_when: "false"
`

const serialBlockWithScope = `name: create
tasks:
  - name: Roll change and check
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: Change
        module: core.exec.run
        params: { cmd: echo, args: ["change"] }
        changed_when: "false"
      - name: Check
        module: core.exec.run
        params: { cmd: echo, args: ["check"] }
        changed_when: "false"
`

// The long-runner/scenario/serial_waves shape: keeper-side captures after a serial
// host task share no wave with it.
const serialThenKeeperCaptures = `name: create
tasks:
  - name: Serial wave
    module: core.exec.run
    serial: 3
    params: { cmd: echo, args: ["wave"] }
    changed_when: "false"
  - name: Record it
    module: core.state.set
    params: { field: recorded, value: "yes" }
  - name: Record again
    module: core.state.set
    params: { field: captured, value: "yes" }
`

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

func seedSerialScopeRun(t *testing.T) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
}

func TestIntegration_SerialScope_RunPathRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, scenario, reason string
	}{
		{"block without a written serial_scope", serialBlockWithoutScope, config.CodeSerialScopeImplicit},
		{"serial pair across tasks", serialPairAcrossTasks, config.CodeSerialScopeImplicitAcrossTasks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedSerialScopeRun(t)
			disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
			applyID := startSerialScopeRun(t, writeServiceRepo(t, tc.scenario), disp)

			inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
			if inc.StatusDetails["reason"] != tc.reason {
				t.Errorf("reason = %v, want %s", inc.StatusDetails["reason"], tc.reason)
			}
			if disp.calls != 0 {
				t.Errorf("SendApply calls = %d, want 0 (refused before any dispatch)", disp.calls)
			}
		})
	}
}

func TestIntegration_SerialScope_RunPathAccepts(t *testing.T) {
	for _, tc := range []struct{ name, scenario string }{
		{"a written serial_scope", serialBlockWithScope},
		{"keeper-side captures after a serial task", serialThenKeeperCaptures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedSerialScopeRun(t)
			disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
			applyID := startSerialScopeRun(t, writeServiceRepo(t, tc.scenario), disp)

			waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
			if disp.calls == 0 {
				t.Error("SendApply was never called: the run did not dispatch")
			}
		})
	}
}

// An install step the Keeper synthesizes from modules[] lands in its consumer's
// Passage; the author never wrote it and soul-lint never sees it, so it must not
// make a single serial: task "share a wave".
func TestIntegration_SerialScope_SynthesizedInstallIsNotTheAuthors(t *testing.T) {
	seedSerialScopeRun(t)
	gitURL := moduleServiceRepo(t, `name: create
tasks:
  - name: Use the echo plugin, one host at a time
    module: community.echo.run
    serial: 1
    params:
      message: hello
`, nil)
	disp := &taskCaptureDispatcher{t: t, result: applyrun.StatusSuccess}
	applyID := startSerialScopeRun(t, gitURL, disp)

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if got := installIndexes(disp.captured()); len(got) != 1 {
		t.Fatalf("synthesized installs = %v, want exactly one beside the consumer", got)
	}
}
