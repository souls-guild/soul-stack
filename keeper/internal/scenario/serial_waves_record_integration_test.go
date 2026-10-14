//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// The waves a serial: step rolled its hosts in are kept on its plan rows (NIM-996),
// and the run view draws the rollout from them alone. So they must be the waves the
// hosts were SENT in, recorded before the first of them, they must keep the hosts of
// a wave the run ended before, and a step sent in one wave must carry none.

const (
	waveHostA = "host-a.example.com"
	waveHostB = "host-b.example.com"
	waveHostC = "host-c.example.com"
	waveHostD = "host-d.example.com"
)

// wavesProbeDispatcher reads the step's recorded waves at its first send: what the
// record says before any host of the step has been sent anything.
type wavesProbeDispatcher struct {
	*serialStagedDispatcher

	mu      sync.Mutex
	atFirst []byte
	probed  bool
}

func (d *wavesProbeDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	if req.GetPassage() == 1 {
		d.mu.Lock()
		if !d.probed {
			d.probed = true
			plan, err := applyrun.SelectRunPlanByApplyID(ctx, integrationPool, req.GetApplyId())
			if err != nil {
				d.t.Errorf("SelectRunPlanByApplyID at the first send: %v", err)
			}
			for _, p := range plan {
				if p.Passage == 1 {
					d.atFirst = p.Waves
					break
				}
			}
		}
		d.mu.Unlock()
	}
	return d.serialStagedDispatcher.SendApply(ctx, sid, req)
}

func runSerialWaves(t *testing.T, gitURL string, sids []string, roles map[string]string, failOn string, wantStatus incarnation.Status) (*wavesProbeDispatcher, map[string]applyrun.RunPlanTask) {
	t.Helper()
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{"redis-prod"})
	}
	disp := &wavesProbeDispatcher{serialStagedDispatcher: newSerialStagedDispatcher(t, roles)}
	disp.failOn = failOn
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "redis-prod", applyID, wantStatus)

	plan, err := applyrun.SelectRunPlanByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunPlanByApplyID: %v", err)
	}
	byName := map[string]applyrun.RunPlanTask{}
	for _, p := range plan {
		byName[p.Name] = p
	}
	return disp, byName
}

func decodeWaves(t *testing.T, raw []byte) [][]string {
	t.Helper()
	if raw == nil {
		return nil
	}
	var waves [][]string
	if err := json.Unmarshal(raw, &waves); err != nil {
		t.Fatalf("waves %q: %v", raw, err)
	}
	return waves
}

func TestIntegration_SerialWaves_RecordedAsDispatched(t *testing.T) {
	disp, plan := runSerialWaves(t, serialStagedServiceRepo(t), []string{waveHostA, waveHostB, waveHostC},
		map[string]string{waveHostA: "slave", waveHostB: "slave", waveHostC: "slave"},
		waveHostB, incarnation.StatusErrorLocked)

	if raw := plan["Probe role"].Waves; raw != nil {
		t.Errorf("probe step (no serial:, one wave) waves = %s, want NULL", raw)
	}
	waves := decodeWaves(t, plan["Rolling-restart replicas one at a time"].Waves)
	want := [][]string{{waveHostA}, {waveHostB}, {waveHostC}}
	if !reflect.DeepEqual(waves, want) {
		t.Fatalf("serial:1 step waves = %v, want %v", waves, want)
	}
	if got := decodeWaves(t, disp.atFirst); !reflect.DeepEqual(got, want) {
		t.Fatalf("waves read at the step's first send = %v, want %v: the record must precede the rollout", got, want)
	}

	// The record is the sends, wave by wave: the hosts sent the step are the waves
	// read in order, up to where the failure on host-b stopped the rollout.
	var flat []string
	for _, w := range waves {
		flat = append(flat, w...)
	}
	sent := disp.passageEvents(1)
	if len(sent) != 2 || !reflect.DeepEqual(sent, flat[:len(sent)]) {
		t.Fatalf("hosts sent the serial step = %v, want the first two of %v", sent, flat)
	}
	// host-c was sent nothing and is still in the record: that is the only place the
	// view learns of a host the rollout never reached.
	if contains(sent, waveHostC) {
		t.Fatalf("host-c was sent the step after the fail-stop: %v", sent)
	}
}

// Only the hosts the step targets ride its waves: a host its where: filters out is in
// none, and the waves close up around it.
func TestIntegration_SerialWaves_OnlyTargetedHosts(t *testing.T) {
	disp, plan := runSerialWaves(t, serialStagedServiceRepo(t), []string{waveHostA, waveHostB, waveHostC},
		map[string]string{waveHostA: "master", waveHostB: "slave", waveHostC: "slave"},
		"", incarnation.StatusReady)

	waves := decodeWaves(t, plan["Rolling-restart replicas one at a time"].Waves)
	want := [][]string{{waveHostB}, {waveHostC}}
	if !reflect.DeepEqual(waves, want) {
		t.Fatalf("waves = %v, want %v (host-a is filtered out by where:)", waves, want)
	}
	if sent := disp.passageEvents(1); !reflect.DeepEqual(sent, []string{waveHostB, waveHostC}) {
		t.Fatalf("hosts sent the serial step = %v, want [host-b host-c]", sent)
	}
}

// The step's `when:` reads the probe's register, so the Keeper settles it per host:
// on the master it is false, and the Keeper closes that host before the first wave,
// sending nothing — it takes no wave's place. On host-c `int()` of a non-number fails
// to evaluate, so the Keeper fails the host before sending it anything — but at its
// turn, in its wave, the way a Soul's failure would land there.
const serialWavesSettledScenario = `name: restart
description: probe role, then one host at a time, the condition settled by the Keeper
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Rolling-restart one at a time
    module: core.exec.run
    when: "register.role.stdout != 'master' && int(register.role.stdout) > 0"
    serial: 1
    settings: { serial_scope: task }
    params:
      cmd: restart
`

func TestIntegration_SerialWaves_KeeperSettledHosts(t *testing.T) {
	disp, plan := runSerialWaves(t, writeServiceRepoScenario(t, "restart", serialWavesSettledScenario),
		[]string{waveHostA, waveHostB, waveHostC, waveHostD},
		map[string]string{waveHostA: "master", waveHostB: "1", waveHostC: "not-a-number", waveHostD: "1"},
		"", incarnation.StatusErrorLocked)

	waves := decodeWaves(t, plan["Rolling-restart one at a time"].Waves)
	want := [][]string{{waveHostB}, {waveHostC}, {waveHostD}}
	if !reflect.DeepEqual(waves, want) {
		t.Fatalf("waves = %v, want %v (host-a closed by the Keeper is in none; host-c, failed before its send, keeps its wave)", waves, want)
	}
	if got := decodeWaves(t, disp.atFirst); !reflect.DeepEqual(got, want) {
		t.Fatalf("waves read at the step's first send = %v, want %v", got, want)
	}
	if sent := disp.passageEvents(1); !reflect.DeepEqual(sent, []string{waveHostB}) {
		t.Fatalf("hosts sent the serial step = %v, want [host-b]: host-c fails at its turn and ends the run", sent)
	}
}
