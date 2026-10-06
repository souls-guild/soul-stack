//go:build integration

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

const applierReload = "Re-read the rotated TLS material"

// crossPassageApplierServiceRepo is rotate_tls with the reload pushed into a
// later Passage than the applier it reacts to: the reload reads the probe
// register in `where:`, the applier does not.
func crossPassageApplierServiceRepo(t *testing.T) string {
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
	write("service.yml", `description: cross-passage applier source
state_schema: {}
destiny:
  - { name: pilot-flat, ref: master }
`)
	write("scenario/rotate_tls/main.yml", `name: rotate_tls
description: applier in passage 0, its onchanges consumer in passage 1
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Re-render TLS PEM material
    register: tls_certs
    apply:
      destiny: pilot-flat
      input:
        marker_file: "/etc/df/tls/dragonfly.crt"
        marker_payload: "pem"
  - name: `+applierReload+`
    module: core.exec.run
    where: "register.role.stdout == 'primary'"
    onchanges: [tls_certs]
    params: { cmd: config-set-tls }
`)
	commitRepo(t, repo)
	return "file://" + dir
}

// applierSoulDispatcher answers Passage 0 the way a real Soul does for an
// applier: each child reports its own status, and the terminal (aggregate_of)
// reports OK with the children folded into its register_data only. Passage 1
// records what reached each host.
type applierSoulDispatcher struct {
	t                 *testing.T
	childChangedOn    map[string]bool // sid → the first destiny child finished CHANGED
	mu                sync.Mutex
	passage1ByHost    map[string][]string
	passage1OnChanges map[string][]int32
}

func (d *applierSoulDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	applyID := req.GetApplyId()
	passage := int(req.GetPassage())
	if passage == 0 {
		var anyChildChanged bool
		for localIdx, task := range req.GetTasks() {
			status := keeperv1.TaskStatus_TASK_STATUS_OK
			data := map[string]any{"changed": false, "failed": false}
			switch {
			case task.GetRegister() == "role":
				data["stdout"] = "primary"
			case len(task.GetAggregateOf()) > 0:
				data["changed"] = anyChildChanged
			case task.GetName() == "Lay down the marker file" && d.childChangedOn[sid]:
				status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
				data["changed"] = true
				anyChildChanged = true
			}
			if task.GetRegister() != "" {
				if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
					ApplyID: applyID, SID: sid, PlanIndex: int(task.GetPlanIndex()), TaskIdx: localIdx, RegisterData: data,
				}); err != nil {
					d.t.Errorf("UpsertTaskRegister: %v", err)
				}
			}
			if err := auditpg.NewWriter(integrationPool).Write(ctx, &audit.Event{
				EventType:     audit.EventTaskExecuted,
				Source:        audit.SourceSoulGRPC,
				CorrelationID: applyID,
				Payload: audit.BuildTaskExecutedPayload(audit.TaskExecutedInput{
					SID: sid, ApplyID: applyID, TaskIdx: localIdx, PlanIndex: int(task.GetPlanIndex()),
					Status: status.String(), Passage: passage,
				}),
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				d.t.Errorf("audit Write: %v", err)
			}
		}
	} else {
		d.mu.Lock()
		for _, task := range req.GetTasks() {
			d.passage1ByHost[sid] = append(d.passage1ByHost[sid], task.GetName())
			if task.GetName() == applierReload {
				d.passage1OnChanges[sid] = task.GetOnchangesIdx()
			}
		}
		d.mu.Unlock()
	}
	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

// TestIntegration_CrossPassageOnChanges_ApplierSource is the end-to-end half
// of the NIM-931 guard: real audit rows, real stage loop, real destiny load.
// A child of tls_certs changed on host-a only, so the reload reaches host-a
// and nothing reaches host-b. Before the fix the terminal's OK status decided,
// and the reload reached neither.
func TestIntegration_CrossPassageOnChanges_ApplierSource(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "df-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"df-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"df-prod"})
	serviceURL := crossPassageApplierServiceRepo(t)
	destinySrc := NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(pilotFlatDestinyRepo(t)))

	disp := &applierSoulDispatcher{
		t:                 t,
		childChangedOn:    map[string]bool{"host-a.example.com": true},
		passage1ByHost:    map[string][]string{},
		passage1OnChanges: map[string][]int32{},
	}
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	r := NewRunner(Deps{
		Loader:       artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:     topology.NewResolver(integrationPool, nil, nil),
		ServiceVars:  servicevars.NewResolver(nil),
		Render:       render.NewPipeline(nil, engine, nil, nil),
		Outbound:     disp,
		Destiny:      destinySrc,
		DB:           integrationPool,
		Audit:        auditpg.NewWriter(integrationPool),
		AuditReader:  auditpg.NewReader(integrationPool),
		SoulCap:      stubSoulCap{},
		PollInterval: 20 * time.Millisecond,
		RunTimeout:   20 * time.Second,
	})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "df-prod",
		ServiceRef:      artifact.ServiceRef{Name: "df", Git: serviceURL, Ref: "master"},
		ScenarioName:    "rotate_tls",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "df-prod", applyID, incarnation.StatusReady)

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if got := disp.passage1ByHost["host-a.example.com"]; !slices.Equal(got, []string{applierReload}) {
		t.Errorf("host-a Passage 1 = %v, want [%s] (a child of tls_certs changed there)", got, applierReload)
	}
	if idx := disp.passage1OnChanges["host-a.example.com"]; len(idx) != 0 {
		t.Errorf("host-a reload onchanges_idx = %v, want [] (resolved Keeper-side)", idx)
	}
	if got, ok := disp.passage1ByHost["host-b.example.com"]; ok {
		t.Errorf("host-b Passage 1 = %v, want nothing dispatched (no child of tls_certs changed there)", got)
	}
}
