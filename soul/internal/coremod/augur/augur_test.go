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

// applyStream — fake grpc.ServerStreamingServer[ApplyEvent] with a settable
// Context (needed to thread the Augur client through stream.Context()).
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

// fakeFetcher emulates soulaugur.Fetcher without a live session.
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
		t.Fatal("Validate ok=true for an unknown verb")
	}
}

func TestValidate_RequiresOmenAndQuery(t *testing.T) {
	m := augurmod.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate ok=true without omen/query")
	}
	if len(reply.Errors) < 2 {
		t.Fatalf("expected errors on omen and query, got %v", reply.Errors)
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
		t.Fatalf("Apply returned an error: %v", err)
	}
	ev := s.last()
	if ev == nil {
		t.Fatal("no ApplyEvent")
	}
	if ev.GetFailed() {
		t.Fatalf("Apply failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Fatal("read-probe must have changed=false")
	}
	if ev.GetOutput().GetFields()["value"].GetStringValue() != "s3cr3t" {
		t.Fatalf("inline_data did not end up in register: %v", ev.GetOutput())
	}
	// The run context was correctly threaded through to Fetch.
	if ff.gotApply != "apply-42" || ff.gotOmen != "vault-prod" {
		t.Fatalf("Fetch got the wrong context: apply=%q omen=%q", ff.gotApply, ff.gotOmen)
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
		t.Fatalf("map inline_data was not preserved as-is: %v", out)
	}
}

// --- Apply: errors ---

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
		t.Fatal("DENIED must produce a failed step")
	}
	// query must not leak into the message (may carry a secret path).
	if got := ev.GetMessage(); strings.Contains(got, "secret/forbidden") {
		t.Fatalf("error message leaks the query: %q", got)
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
		t.Fatal("ERROR must produce a failed step")
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
		t.Fatal("a closed client must produce a failed step")
	}
}

func TestApply_AugurUnavailable_IsStepError(t *testing.T) {
	// ctx WITHOUT Augur plumbing (push mode / session without Augur).
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
		t.Fatal("without an Augur client the step must fail with a clear error")
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
		t.Fatal("unknown verb must fail")
	}
}

func TestApply_MissingParams(t *testing.T) {
	ff := &fakeFetcher{}
	s := &applyStream{ctx: ctxWith(ff, "apply-1")}
	m := augurmod.New()
	req := &pluginv1.ApplyRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{"omen": "o"}), // no query
	}
	if err := m.Apply(req, s); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !s.last().GetFailed() {
		t.Fatal("missing query must fail")
	}
	if ff.callCount != 0 {
		t.Fatal("Fetch must not be called with invalid params")
	}
}
