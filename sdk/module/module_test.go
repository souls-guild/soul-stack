package module

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// TestBaseModuleValidateOk checks that the BaseModule.Validate default
// returns Ok=true and an empty error list (no-op before the author overrides it).
func TestBaseModuleValidateOk(t *testing.T) {
	var b BaseModule
	reply, err := b.Validate(context.Background(), &pluginv1.ValidateRequest{State: "installed"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("Validate reply=%+v", reply)
	}
}

// TestBaseModulePlanEmpty checks that the BaseModule.Plan default sends no events and returns nil.
func TestBaseModulePlanEmpty(t *testing.T) {
	var b BaseModule
	stream := &fakePlanStream{}
	if err := b.Plan(&pluginv1.PlanRequest{}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("Plan sent %d events, want 0", len(stream.sent))
	}
}

// TestBaseModuleApplyEmpty checks that the BaseModule.Apply default is also a
// no-op (the plugin author must override it; fine for smoke tests).
func TestBaseModuleApplyEmpty(t *testing.T) {
	var b BaseModule
	stream := &fakeApplyStream{}
	if err := b.Apply(&pluginv1.ApplyRequest{}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(stream.sent) != 0 {
		t.Fatalf("Apply sent %d events, want 0", len(stream.sent))
	}
}

// TestServerAdapterDelegates verifies that the internal adapter proxies calls
// to the user impl with the right parameters and propagates errors.
func TestServerAdapterDelegates(t *testing.T) {
	wantErr := errors.New("boom")
	impl := &fakeModule{
		validateReply: &pluginv1.ValidateReply{Ok: false, Errors: []string{"x"}},
		planErr:       wantErr,
	}
	adapter := &serverAdapter{impl: impl}

	reply, err := adapter.Validate(context.Background(), &pluginv1.ValidateRequest{State: "running"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if impl.validateState != "running" {
		t.Fatalf("Validate state=%q", impl.validateState)
	}
	if reply.Ok || reply.Errors[0] != "x" {
		t.Fatalf("Validate reply=%+v", reply)
	}

	if err := adapter.Plan(&pluginv1.PlanRequest{State: "p"}, &fakePlanStream{}); !errors.Is(err, wantErr) {
		t.Fatalf("Plan err=%v want %v", err, wantErr)
	}

	if err := adapter.Apply(&pluginv1.ApplyRequest{State: "a"}, &fakeApplyStream{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if impl.applyState != "a" {
		t.Fatalf("Apply state=%q", impl.applyState)
	}
}

// TestBaseModuleNotPlanReadSafe checks that BaseModule DELIBERATELY doesn't
// implement PlanReadSafe: a plugin built on BaseModule gets default-deny on
// dry_run by default (ADR-031), instead of silently reporting "no drift".
func TestBaseModuleNotPlanReadSafe(t *testing.T) {
	var b any = BaseModule{}
	if _, ok := b.(PlanReadSafe); ok {
		t.Fatal("BaseModule реализует PlanReadSafe — должен НЕ реализовывать (default-deny)")
	}
	var p any = &planReadSafeImpl{}
	if _, ok := p.(PlanReadSafe); !ok {
		t.Fatal("явная реализация PlanReadSafe не распознаётся type-assertion-ом")
	}
}

// planReadSafeImpl is a module that declares a read-safe Plan.
type planReadSafeImpl struct{ BaseModule }

func (planReadSafeImpl) PlanReadSafe() {}

// TestBaseModuleNotErrandReadSafe checks that BaseModule DELIBERATELY doesn't
// implement ErrandReadSafe: a plugin built on BaseModule gets default-deny on
// an Errand call by default (ADR-033), instead of silently running as ad-hoc
// on the host.
func TestBaseModuleNotErrandReadSafe(t *testing.T) {
	var b any = BaseModule{}
	if _, ok := b.(ErrandReadSafe); ok {
		t.Fatal("BaseModule реализует ErrandReadSafe — должен НЕ реализовывать (default-deny)")
	}
	var e any = &errandReadSafeImpl{}
	if _, ok := e.(ErrandReadSafe); !ok {
		t.Fatal("явная реализация ErrandReadSafe не распознаётся type-assertion-ом")
	}
}

// errandReadSafeImpl is a module that declares a read-safe Apply (Errand-safe).
type errandReadSafeImpl struct{ BaseModule }

func (errandReadSafeImpl) ErrandReadSafe() {}

// fakeModule is a mock implementation of SoulModule for adapter tests.
type fakeModule struct {
	validateState string
	planErr       error
	applyState    string
	validateReply *pluginv1.ValidateReply
}

func (f *fakeModule) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	f.validateState = req.State
	return f.validateReply, nil
}

func (f *fakeModule) Plan(req *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return f.planErr
}

func (f *fakeModule) Apply(req *pluginv1.ApplyRequest, _ grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	f.applyState = req.State
	return nil
}

// fakePlanStream / fakeApplyStream are minimal mock grpc.ServerStreamingServer
// implementations for in-process unit tests (without spinning up a real grpc server).
type fakePlanStream struct {
	grpc.ServerStreamingServer[pluginv1.PlanEvent]
	sent []*pluginv1.PlanEvent
}

func (s *fakePlanStream) Send(e *pluginv1.PlanEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func (s *fakePlanStream) Context() context.Context { return context.Background() }

type fakeApplyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	sent []*pluginv1.ApplyEvent
}

func (s *fakeApplyStream) Send(e *pluginv1.ApplyEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func (s *fakeApplyStream) Context() context.Context { return context.Background() }
