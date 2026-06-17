package augur_test

import (
	"context"
	"strings"
	"testing"

	soulaugur "github.com/souls-guild/soul-stack/soul/internal/augur"
	augurmod "github.com/souls-guild/soul-stack/soul/internal/coremod/augur"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyStream — fake grpc.ServerStreamingServer[ApplyEvent] с настраиваемым
// Context (нужен, чтобы прокинуть Augur-клиент через stream.Context()).
type applyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	ctx    context.Context
	events []*pluginv1.ApplyEvent
}

func (s *applyStream) Send(e *pluginv1.ApplyEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *applyStream) Context() context.Context { return s.ctx }

func (s *applyStream) last() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}

// fakeFetcher эмулирует soulaugur.Fetcher без живой сессии.
type fakeFetcher struct {
	reply     *keeperv1.AugurReply
	err       error
	gotApply  string
	gotOmen   string
	gotQuery  string
	callCount int
}

func (f *fakeFetcher) Fetch(_ context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error) {
	f.callCount++
	f.gotApply = applyID
	f.gotOmen = omen
	f.gotQuery = query
	return f.reply, f.err
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func ctxWith(f soulaugur.Fetcher, applyID string) context.Context {
	return soulaugur.WithRun(context.Background(), f, applyID)
}

// --- Validate ---

func TestValidate_OK(t *testing.T) {
	m := augurmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "vault-prod", "query": "secret/data/app#token"}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok=false: %v", reply.Errors)
	}
}

func TestValidate_RejectsUnknownVerb(t *testing.T) {
	m := augurmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "read",
		Params: mustStruct(t, map[string]any{"omen": "o", "query": "q"}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true для неизвестного verb")
	}
}

func TestValidate_RequiresOmenAndQuery(t *testing.T) {
	m := augurmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true без omen/query")
	}
	if len(reply.Errors) < 2 {
		t.Fatalf("ожидались ошибки по omen и query, got %v", reply.Errors)
	}
}

// --- Apply: OK ---

func TestApply_OK_InlineDataToRegister(t *testing.T) {
	ff := &fakeFetcher{
		reply: &keeperv1.AugurReply{
			Status: keeperv1.AugurStatus_AUGUR_STATUS_OK,
			Result: &keeperv1.AugurReply_InlineData{
				InlineData: mustStruct(t, map[string]any{"value": "s3cr3t"}),
			},
		},
	}
	s := &applyStream{ctx: ctxWith(ff, "apply-42")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "vault-prod", "query": "secret/data/app#token"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply вернул ошибку: %v", err)
	}
	ev := s.last()
	if ev == nil {
		t.Fatal("нет ApplyEvent")
	}
	if ev.GetFailed() {
		t.Fatalf("Apply failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Fatal("read-probe обязан быть changed=false")
	}
	if ev.GetOutput().GetFields()["value"].GetStringValue() != "s3cr3t" {
		t.Fatalf("inline_data не попал в register: %v", ev.GetOutput())
	}
	// Контекст прогона корректно прокинут в Fetch.
	if ff.gotApply != "apply-42" || ff.gotOmen != "vault-prod" {
		t.Fatalf("Fetch получил неверный контекст: apply=%q omen=%q", ff.gotApply, ff.gotOmen)
	}
}

func TestApply_OK_MapInlineData(t *testing.T) {
	ff := &fakeFetcher{
		reply: &keeperv1.AugurReply{
			Status: keeperv1.AugurStatus_AUGUR_STATUS_OK,
			Result: &keeperv1.AugurReply_InlineData{
				InlineData: mustStruct(t, map[string]any{"user": "admin", "port": 5432}),
			},
		},
	}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "vault-prod", "query": "secret/data/db"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := s.last().GetOutput().AsMap()
	if out["user"] != "admin" {
		t.Fatalf("map inline_data не сохранён as-is: %v", out)
	}
}

// --- Apply: ошибки ---

func TestApply_Denied_IsStepError(t *testing.T) {
	ff := &fakeFetcher{err: soulaugur.ErrDenied}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "vault-prod", "query": "secret/forbidden"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := s.last()
	if !ev.GetFailed() {
		t.Fatal("DENIED должен давать failed-шаг")
	}
	// query не должен светиться в сообщении (может нести путь к секрету).
	if got := ev.GetMessage(); strings.Contains(got, "secret/forbidden") {
		t.Fatalf("сообщение об ошибке светит query: %q", got)
	}
}

func TestApply_RemoteError_IsStepError(t *testing.T) {
	ff := &fakeFetcher{err: soulaugur.ErrRemote}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "prom-main", "query": "up"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !s.last().GetFailed() {
		t.Fatal("ERROR должен давать failed-шаг")
	}
}

func TestApply_ClientClosed_IsStepError(t *testing.T) {
	ff := &fakeFetcher{err: soulaugur.ErrClientClosed}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "o", "query": "q"}),
	}
	_ = m.Apply(req, s)
	if !s.last().GetFailed() {
		t.Fatal("закрытый клиент должен давать failed-шаг")
	}
}

func TestApply_AugurUnavailable_IsStepError(t *testing.T) {
	// ctx БЕЗ Augur-плумбинга (push-режим / сессия без Augur).
	s := &applyStream{ctx: context.Background()}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "o", "query": "q"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !s.last().GetFailed() {
		t.Fatal("без Augur-клиента шаг должен падать понятной ошибкой")
	}
}

func TestApply_UnknownVerb(t *testing.T) {
	s := &applyStream{ctx: ctxWith(&fakeFetcher{}, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "read",
		Params: mustStruct(t, map[string]any{"omen": "o", "query": "q"}),
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !s.last().GetFailed() {
		t.Fatal("неизвестный verb должен падать")
	}
}

func TestApply_MissingParams(t *testing.T) {
	ff := &fakeFetcher{}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "o"}), // нет query
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !s.last().GetFailed() {
		t.Fatal("отсутствие query должно падать")
	}
	if ff.callCount != 0 {
		t.Fatal("Fetch не должен вызываться при невалидных params")
	}
}
