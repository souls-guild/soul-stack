package choir_test

import (
	"context"
	"errors"
	"testing"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	coremodchoir "github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeStore is a deterministic in-memory Store for the module's unit tests.
// Captures AddVoice/RemoveVoice calls to assert on side effects.
type fakeStore struct {
	addErr    error
	removeErr error
	existsErr error
	notExists bool

	addCalls    int
	removeCalls int
	lastVoice   *keeperchoir.Voice
	lastRemove  [3]string
}

func (s *fakeStore) AddVoice(_ context.Context, v *keeperchoir.Voice) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.addCalls++
	cp := *v
	s.lastVoice = &cp
	return nil
}

func (s *fakeStore) RemoveVoice(_ context.Context, incarnation, choirName, sid string) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.removeCalls++
	s.lastRemove = [3]string{incarnation, choirName, sid}
	return nil
}

func (s *fakeStore) IncarnationExists(_ context.Context, _ string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return !s.notExists, nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func baseParams() map[string]any {
	return map[string]any{
		"incarnation": "redis-prod",
		"choir":       "masters",
		"sid":         "h1.example.com",
	}
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

func TestValidate_OK(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, baseParams()),
	})
	if !rep.Ok {
		t.Fatalf("expected Ok=true, errors=%v", rep.Errors)
	}
}

func TestValidate_EmptyStateOK(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		Params: mustStruct(t, baseParams()),
	})
	if !rep.Ok {
		t.Fatalf("empty state must default to present, errors=%v", rep.Errors)
	}
}

func TestValidate_UnknownState(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "registered",
		Params: mustStruct(t, baseParams()),
	})
	if rep.Ok {
		t.Fatalf("expected Ok=false on unknown state")
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatalf("expected errors")
	}
	if len(rep.Errors) < 3 {
		t.Fatalf("expected at least 3 errors (incarnation/choir/sid), got %v", rep.Errors)
	}
}

func TestValidate_NegativePosition(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	p := baseParams()
	p["position"] = float64(-1)
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "present",
		Params: mustStruct(t, p),
	})
	if rep.Ok {
		t.Fatalf("expected Ok=false on negative position")
	}
}

// ---------------------------------------------------------------------------
// Apply present → AddVoice
// ---------------------------------------------------------------------------

func TestApply_Present_AddsVoice(t *testing.T) {
	fs := &fakeStore{}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()

	p := baseParams()
	p["role"] = "seed"
	p["position"] = float64(0)
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, p)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("unexpected: %+v", ev)
	}
	if !ev.Changed {
		t.Fatal("expected changed=true on add")
	}
	if fs.addCalls != 1 {
		t.Fatalf("addCalls=%d, want 1", fs.addCalls)
	}
	if fs.lastVoice.Role == nil || *fs.lastVoice.Role != "seed" {
		t.Errorf("role not passed: %+v", fs.lastVoice.Role)
	}
	if fs.lastVoice.Position == nil || *fs.lastVoice.Position != 0 {
		t.Errorf("position not passed: %+v", fs.lastVoice.Position)
	}
	out := ev.Output.AsMap()
	if out["added"] != true || out["state"] != "present" {
		t.Errorf("output=%v", out)
	}
}

func TestApply_Present_EmptyStateDefaults(t *testing.T) {
	fs := &fakeStore{}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("empty state should default to present, got %+v", stream.Last())
	}
	if fs.addCalls != 1 {
		t.Fatalf("addCalls=%d, want 1", fs.addCalls)
	}
}

func TestApply_Present_Idempotent_VoiceExists(t *testing.T) {
	fs := &fakeStore{addErr: keeperchoir.ErrVoiceExists}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("ErrVoiceExists must be idempotent no-op, got failed: %+v", ev)
	}
	if ev.Changed {
		t.Fatal("expected changed=false on idempotent add")
	}
	if ev.Output.AsMap()["added"] != false {
		t.Errorf("added should be false, got %v", ev.Output.AsMap()["added"])
	}
}

func TestApply_Present_NotMembers_Fails(t *testing.T) {
	fs := &fakeStore{addErr: &keeperchoir.ErrNotMembers{
		Incarnation: "redis-prod",
		Missing:     []string{"h1.example.com"},
	}}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("ErrNotMembers must produce failed-event")
	}
}

func TestApply_Present_ChoirNotFound_Fails(t *testing.T) {
	fs := &fakeStore{addErr: keeperchoir.ErrChoirNotFound}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("ErrChoirNotFound must produce failed-event")
	}
}

// ---------------------------------------------------------------------------
// Apply absent → RemoveVoice
// ---------------------------------------------------------------------------

func TestApply_Absent_RemovesVoice(t *testing.T) {
	fs := &fakeStore{}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "absent", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("expected changed=true success, got %+v", ev)
	}
	if fs.removeCalls != 1 {
		t.Fatalf("removeCalls=%d, want 1", fs.removeCalls)
	}
	if fs.lastRemove != [3]string{"redis-prod", "masters", "h1.example.com"} {
		t.Errorf("removed args=%v", fs.lastRemove)
	}
	if ev.Output.AsMap()["removed"] != true {
		t.Errorf("removed should be true")
	}
}

func TestApply_Absent_Idempotent_VoiceNotFound(t *testing.T) {
	fs := &fakeStore{removeErr: keeperchoir.ErrVoiceNotFound}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "absent", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("ErrVoiceNotFound must be idempotent no-op, got failed: %+v", ev)
	}
	if ev.Changed {
		t.Fatal("expected changed=false on idempotent remove")
	}
}

func TestApply_Absent_RemoveError_Fails(t *testing.T) {
	fs := &fakeStore{removeErr: errors.New("boom")}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "absent", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed-event on remove error")
	}
}

// ---------------------------------------------------------------------------
// Validation / guards in Apply
// ---------------------------------------------------------------------------

func TestApply_UnknownState_Fails(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "registered", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on unknown state")
	}
}

func TestApply_InvalidSID_Fails(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	stream := internaltest.NewApplyStream()
	p := baseParams()
	p["sid"] = "BAD.HOST"
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, p)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on invalid sid")
	}
}

func TestApply_InvalidChoirName_Fails(t *testing.T) {
	m := coremodchoir.New(&fakeStore{})
	stream := internaltest.NewApplyStream()
	p := baseParams()
	p["choir"] = "Masters" // not kebab/snake, starts with uppercase
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, p)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed on invalid choir name")
	}
}

func TestApply_IncarnationNotFound_Fails(t *testing.T) {
	fs := &fakeStore{notExists: true}
	m := coremodchoir.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "present", Params: mustStruct(t, baseParams())}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed when incarnation not found")
	}
	if fs.addCalls != 0 {
		t.Errorf("AddVoice must not be called when incarnation missing")
	}
}
