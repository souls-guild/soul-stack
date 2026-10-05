//go:build integration

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	coremodssh "github.com/souls-guild/soul-stack/keeper/internal/coremod/ssh"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// renderProbeModule stands in for `core.ssh` and renders the destiny it was asked
// for through whatever the run put on its context — the one thing this test is
// about. Nothing is dialed.
type renderProbeModule struct {
	module.BaseModule
	mu       sync.Mutex
	rendered *coremodutil.RenderedDestiny
	err      error
}

func (m *renderProbeModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	renderDestiny := coremodutil.DestinyRendererFrom(ctx)
	if renderDestiny == nil {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "no destiny render on the module context"})
	}
	p := req.GetParams().AsMap()
	input, _ := p["input"].(map[string]any)
	rd, err := renderDestiny(ctx, p["destiny"].(string), "vm-1.example", input)
	m.mu.Lock()
	m.rendered, m.err = rd, err
	m.mu.Unlock()
	if err != nil {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: err.Error()})
	}
	return stream.Send(&pluginv1.ApplyEvent{})
}

func sshApplyServiceRepo(t *testing.T) string {
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
	write("service.yml", `description: core.ssh.apply integration service
state_schema: {}
destiny:
  - { name: pilot-flat, ref: master }
`)
	write("scenario/create/main.yml", `name: create
description: apply pilot-flat to a machine the registry does not hold
tasks:
  - name: Apply pilot-flat over SSH
    module: core.ssh.apply
    params:
      ssh_provider: teleport
      destiny: pilot-flat
      hosts:
        - { sid: vm-1.example }
      input:
        marker_file: /etc/marker
        marker_payload: ok
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init ssh-apply service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// ★ GUARD (NIM-905, decision 6). A scenario run hands `core.ssh.apply` a render
// bound to ITS resolver and incarnation: the destiny comes from the service's
// `destiny[]` at its ref, through the same git loader an `apply:` uses, and is
// rendered for a host that is not in the run's roster — this run has none.
//
// The module-level tests inject the render themselves, so they stay green if the
// run stops supplying it. This is the hop they cannot see.
//
// Mutation: drop the WithDestinyRenderer line in run.go and this reddens — the
// step finds no render and the run ends error_locked.
func TestIntegration_SSHApply_RunHandsTheModuleItsDestinyRender(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "edge-1")

	serviceURL := sshApplyServiceRepo(t)
	destinySrc := NewDestinySource(artifact.NewDestinyLoader(t.TempDir(), nil), fixedTemplateSource(pilotFlatDestinyRepo(t)))

	r := newRunnerWithDestiny(t, &mockDispatcher{t: t, result: applyrun.StatusSuccess}, destinySrc)
	probe := &renderProbeModule{}
	r.keeperModules = keeperRegistryWith(map[string]module.SoulModule{coremodssh.Name: probe})

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "edge-1",
		ServiceRef:      artifact.ServiceRef{Name: "edge", Git: serviceURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "edge-1", applyID, incarnation.StatusReady)

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.err != nil {
		t.Fatalf("render through the run: %v", probe.err)
	}
	if probe.rendered == nil || len(probe.rendered.Tasks) != 2 {
		t.Fatalf("rendered = %+v, want pilot-flat's two tasks", probe.rendered)
	}
	if probe.rendered.Ref != "master" {
		t.Errorf("ref = %q, want the service's declared ref", probe.rendered.Ref)
	}
	if got := probe.rendered.Tasks[0].GetParams().AsMap()["path"]; got != "/etc/marker" {
		t.Errorf("path = %v, want the input rendered in", got)
	}
}
