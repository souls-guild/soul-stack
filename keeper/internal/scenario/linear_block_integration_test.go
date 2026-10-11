//go:build integration

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// linearBlockServiceRepo: one default block whose second child reads the first
// child's register. Under staged dispatch that is within_block_register_dependency;
// under linear dispatch the two are steps 0 and 1.
func linearBlockServiceRepo(t *testing.T) string {
	t.Helper()
	return linearServiceRepo(t, "pair", `name: pair
description: a default block whose second child reads the first child's register
tasks:
  - name: pair
    block:
      - name: a
        module: core.exec.run
        register: a
        changed_when: "false"
        params: { cmd: probe }
      - name: b
        module: core.exec.run
        where: "register.a.stdout == 'yes'"
        params: { cmd: act }
`)
}

func linearServiceRepo(t *testing.T, scenario, body string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", "description: linear block service\nstate_schema: {}\n")
	write("scenario/"+scenario+"/main.yml", body)
	commitRepo(t, repo)
	return "file://" + dir
}

// linearStepDispatcher records which task names each host received per Passage and
// answers a's register per host.
type linearStepDispatcher struct {
	t      *testing.T
	aBySID map[string]string

	mu   sync.Mutex
	sent map[int]map[string][]string // passage → sid → task names
}

func (d *linearStepDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()
	d.mu.Lock()
	if d.sent[passage] == nil {
		d.sent[passage] = map[string][]string{}
	}
	for _, task := range req.GetTasks() {
		d.sent[passage][sid] = append(d.sent[passage][sid], task.GetName())
	}
	d.mu.Unlock()

	if localIdx, planIdx := emitterIndices(req, "a"); localIdx >= 0 {
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx,
			RegisterData: map[string]any{"stdout": d.aBySID[sid], "changed": false, "failed": false},
			Passage:      passage,
		}); err != nil {
			d.t.Errorf("linearStepDispatcher: UpsertTaskRegister(%s): %v", sid, err)
		}
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("linearStepDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *linearStepDispatcher) messages() map[int]map[string][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[int]map[string][]string{}
	for p, bySID := range d.sent {
		out[p] = map[string][]string{}
		for sid, names := range bySID {
			sorted := append([]string(nil), names...)
			sort.Strings(sorted)
			out[p][sid] = sorted
		}
	}
	return out
}

// ★ GUARD (ADR-0091, decision 1, item 12). A run that started linear
// sends a default block's children as two steps: a to both hosts at Passage 0,
// then b — whose where: reads a's register — only to the host a said yes on, at
// Passage 1. The same file run staged is refused within_block_register_dependency
// before anything is sent.
//
// Mutations: number a linear run with Stratify, or leave RenderInput.Steps unset
// → the block is one Passage again and b's where: is resolved before a ran; keep
// the staged within-block check for linear runs → the run is refused.
func TestIntegration_LinearRunSplitsDefaultBlock(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "redis-prod")
			seedConnectedSoul(t, hostA, []string{"redis-prod"})
			seedConnectedSoul(t, hostB, []string{"redis-prod"})
			gitURL := linearBlockServiceRepo(t)

			disp := &linearStepDispatcher{t: t, aBySID: map[string]string{hostA: "yes", hostB: "no"}, sent: map[int]map[string][]string{}}
			r := newRunner(t, disp, gitURL)
			r.deps.ExecutionModel = model

			applyID := audit.NewULID()
			if err := r.Start(context.Background(), RunSpec{
				ApplyID:         applyID,
				IncarnationName: "redis-prod",
				ServiceRef:      artifact.ServiceRef{Name: "redis", Git: gitURL, Ref: "master"},
				ScenarioName:    "pair",
				StartedByAID:    "archon-alice",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			if model == ExecutionStaged {
				inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
				if inc.StatusDetails["reason"] != config.CodeWithinBlockRegisterDependency {
					t.Errorf("staged reason = %v, want %s", inc.StatusDetails["reason"], config.CodeWithinBlockRegisterDependency)
				}
				if got := disp.messages(); len(got) != 0 {
					t.Errorf("staged run sent %v, want nothing", got)
				}
				return
			}

			waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
			want := map[int]map[string][]string{
				0: {hostA: {"a"}, hostB: {"a"}},
				1: {hostA: {"b"}},
			}
			if got := disp.messages(); !reflect.DeepEqual(got, want) {
				t.Errorf("messages = %v, want %v", got, want)
			}
		})
	}
}
