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

// awareModule — fakeModule, реализующий util.SoulprintAware: фиксирует факт,
// инжектнутый runner-ом перед Apply (Вариант A, ADR-018(b)).
type awareModule struct {
	module.BaseModule
	gotFacts util.HostFacts
	awareSet bool
	// applyHostFacts — снимок facts на момент Apply: проверяет, что SetHostFacts
	// вызван ИМЕННО ДО Apply (а не после).
	applyHostFacts util.HostFacts
}

func (a *awareModule) SetHostFacts(f util.HostFacts) {
	a.gotFacts = f
	a.awareSet = true
}

func (a *awareModule) Apply(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	a.applyHostFacts = a.gotFacts // facts уже должны быть инжектнуты
	return stream.Send(&pluginv1.ApplyEvent{Changed: true})
}

// TestRun_InjectsHostFactsIntoAwareModule — runner инжектит собранный soulprint-
// факт в SoulprintAware-модуль ПЕРЕД Apply.
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

// TestRun_NonAwareModule_NoPanic — модуль без util.SoulprintAware не получает
// факт и работает штатно (out-of-process-плагины, Вариант A не трогает контракт).
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
