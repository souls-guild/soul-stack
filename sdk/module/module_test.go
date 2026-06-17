package module

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// TestBaseModuleValidateOk — дефолт BaseModule.Validate возвращает Ok=true
// и пустой список ошибок (no-op перед переопределением автором).
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

// TestBaseModulePlanEmpty — дефолт BaseModule.Plan не шлёт событий и возвращает nil.
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

// TestBaseModuleApplyEmpty — дефолт BaseModule.Apply тоже no-op (плагин-автор
// обязан переопределить; для smoke-test-ов это допустимо).
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

// TestServerAdapterDelegates — verifies, что внутренний adapter проксирует
// вызовы к user-impl с правильными параметрами и пробрасывает ошибки.
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

// TestBaseModuleNotPlanReadSafe — BaseModule СОЗНАТЕЛЬНО не реализует
// PlanReadSafe: плагин на BaseModule по умолчанию получает default-deny на
// dry_run (ADR-031), а не молча выдаёт «нет дрифта».
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

// planReadSafeImpl — модуль, объявивший read-safe Plan.
type planReadSafeImpl struct{ BaseModule }

func (planReadSafeImpl) PlanReadSafe() {}

// TestBaseModuleNotErrandReadSafe — BaseModule СОЗНАТЕЛЬНО не реализует
// ErrandReadSafe: плагин на BaseModule по умолчанию получает default-deny при
// Errand-вызове (ADR-033), а не молча выполняется как ad-hoc на хосте.
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

// errandReadSafeImpl — модуль, объявивший read-safe Apply (Errand-safe).
type errandReadSafeImpl struct{ BaseModule }

func (errandReadSafeImpl) ErrandReadSafe() {}

// fakeModule — mock-implementation SoulModule для adapter-тестов.
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

// fakePlanStream / fakeApplyStream — минимальные mock grpc.ServerStreamingServer
// для in-process unit-тестов (без поднятия реального grpc-server-а).
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
