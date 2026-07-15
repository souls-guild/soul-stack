package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// awareModule is a fakeModule implementing util.SoulprintAware: records the
// fact injected by the runner before Apply (Variant A, ADR-018(b)).
type awareModule struct {
	module.BaseModule
	gotFacts util.HostFacts
	awareSet bool
	// applyHostFacts is a snapshot of facts at Apply time: verifies SetHostFacts
	// is called BEFORE Apply (not after).
	applyHostFacts util.HostFacts
}

func (a *awareModule) SetHostFacts(f util.HostFacts) {
	a.gotFacts = f
	a.awareSet = true
}

func (a *awareModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	a.applyHostFacts = a.gotFacts // facts must already be injected
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// TestRun_InjectsHostFactsIntoAwareModule — the runner injects the collected
// soulprint fact into a SoulprintAware module BEFORE Apply.
func TestRun_InjectsHostFactsIntoAwareModule(t *testing.T) {
	mod := &awareModule{}
	reg := mapRegistry{"core.pkg": mod}
	r := NewApplyRunner(reg, nil)
	r.SetHostFacts(util.HostFacts{PkgMgr: util.PkgMgrApk, InitSystem: util.InitSystemOpenRC})

	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "install", Module: "core.pkg.installed", Params: mustStruct(t, map[string]any{"name": "nginx"})},
		},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !mod.awareSet {
		t.Fatal("SetHostFacts не вызван на SoulprintAware-модуле")
	}
	if mod.applyHostFacts.PkgMgr != util.PkgMgrApk || mod.applyHostFacts.InitSystem != util.InitSystemOpenRC {
		t.Fatalf("на момент Apply facts=%+v, want {apk, openrc} (инжект ДО Apply)", mod.applyHostFacts)
	}
}

// TestRun_NonAwareModule_NoPanic — a module without util.SoulprintAware
// doesn't receive the fact and works normally (out-of-process plugins,
// Variant A doesn't touch the contract).
func TestRun_NonAwareModule_NoPanic(t *testing.T) {
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Changed: true})
			},
		},
	}
	r := NewApplyRunner(reg, nil)
	r.SetHostFacts(util.HostFacts{PkgMgr: util.PkgMgrApk})

	sink := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-1",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "run", Module: "core.exec.run"},
		},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.runResult.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("runResult=%+v, want SUCCESS", sink.runResult)
	}
}
