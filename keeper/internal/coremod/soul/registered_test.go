package soul_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodsoul "github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeStore is a deterministic in-memory Store for the module's unit tests.
// Captures every call to assert on side effects.
type fakeStore struct {
	byID         map[string]*keepersoul.Soul
	insertCalls  int
	updateCalls  int
	lastInserted *keepersoul.Soul
	lastUpdated  string
	lastCoven    []string
	updateErr    error
	insertErr    error
	selectErr    error

	// Soulprint facts for the refresh_soulprint barrier (fakePresence model):
	// factsBySID — SIDs that already have typed soulprint recorded; factsAfter —
	// sid → which SoulsWithSoulprint call the facts start "appearing in PG" on.
	factsBySID map[string]struct{}
	factsAfter map[string]int
	factsCalls int
	factsErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID:       map[string]*keepersoul.Soul{},
		factsBySID: map[string]struct{}{},
		factsAfter: map[string]int{},
	}
}

func (s *fakeStore) SoulsWithSoulprint(_ context.Context, sids []string) (map[string]struct{}, error) {
	if s.factsErr != nil {
		return nil, s.factsErr
	}
	s.factsCalls++
	res := map[string]struct{}{}
	for _, sid := range sids {
		if _, ok := s.factsBySID[sid]; ok {
			res[sid] = struct{}{}
			continue
		}
		if after, ok := s.factsAfter[sid]; ok && s.factsCalls >= after {
			res[sid] = struct{}{}
		}
	}
	return res, nil
}

func (s *fakeStore) SelectBySID(_ context.Context, sid string) (*keepersoul.Soul, error) {
	if s.selectErr != nil {
		return nil, s.selectErr
	}
	v, ok := s.byID[sid]
	if !ok {
		return nil, keepersoul.ErrSoulNotFound
	}
	// Defensive copy so the test doesn't accidentally mutate internal state.
	cp := *v
	cp.Coven = append([]string(nil), v.Coven...)
	return &cp, nil
}

func (s *fakeStore) Insert(_ context.Context, soul *keepersoul.Soul) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	if _, exists := s.byID[soul.SID]; exists {
		return keepersoul.ErrSoulAlreadyExists
	}
	s.insertCalls++
	cp := *soul
	cp.Coven = append([]string(nil), soul.Coven...)
	s.byID[soul.SID] = &cp
	s.lastInserted = &cp
	return nil
}

func (s *fakeStore) UpdateCoven(_ context.Context, sid string, coven []string) ([]string, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	v, ok := s.byID[sid]
	if !ok {
		return nil, keepersoul.ErrSoulNotFound
	}
	s.updateCalls++
	s.lastUpdated = sid
	s.lastCoven = append([]string(nil), coven...)
	v.Coven = append([]string(nil), coven...)
	return append([]string(nil), coven...), nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestValidate_UnknownState(t *testing.T) {
	m := coremodsoul.New(newFakeStore())
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "exists",
		Params: mustStruct(t, map[string]any{"sid": "h.example.com", "coven": []any{"prod"}}),
	})
	if rep.Ok {
		t.Fatalf("expected Ok=false")
	}
}

func TestValidate_MissingSid(t *testing.T) {
	// coven is OPTIONAL now (NIM-124): only sid is required, so missing both
	// yields exactly the sid error.
	m := coremodsoul.New(newFakeStore())
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "registered",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatalf("expected errors (sid required)")
	}
	if len(rep.Errors) != 1 {
		t.Fatalf("expected exactly 1 error (sid; coven is optional), got %v", rep.Errors)
	}
}

func TestValidate_CovenOptional(t *testing.T) {
	// sid only, no coven → valid (NIM-124: coven optional, membership implicit).
	m := coremodsoul.New(newFakeStore())
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "registered",
		Params: mustStruct(t, map[string]any{"sid": "h.example.com"}),
	})
	if !rep.Ok {
		t.Fatalf("expected Ok=true (coven optional), got errors %v", rep.Errors)
	}
}

func TestValidate_UnknownMode(t *testing.T) {
	m := coremodsoul.New(newFakeStore())
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h.example.com",
			"coven": []any{"prod"},
			"mode":  "merge",
		}),
	})
	if rep.Ok {
		t.Fatalf("expected Ok=false on unknown mode")
	}
}

func TestApply_CreatesSoulWhenAbsent_Append(t *testing.T) {
	fs := newFakeStore()
	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod", "db"},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev == nil || ev.Failed {
		t.Fatalf("unexpected: %+v", ev)
	}
	if !ev.Changed {
		t.Fatal("expected changed=true on create")
	}
	if fs.insertCalls != 1 {
		t.Fatalf("insertCalls = %d, want 1", fs.insertCalls)
	}
	if fs.updateCalls != 1 {
		t.Fatalf("updateCalls = %d, want 1", fs.updateCalls)
	}

	out := ev.Output.AsMap()
	if out["created"] != true {
		t.Errorf("created=%v, want true", out["created"])
	}
	if out["mode"] != "append" {
		t.Errorf("mode=%v, want append", out["mode"])
	}
	got, _ := out["coven"].([]any)
	if !reflect.DeepEqual(got, []any{"db", "prod"}) {
		t.Errorf("coven=%v, want [db prod]", got)
	}
}

// TestApply_RefreshSoulprint_OutputTrue — ADR-061 §S3 (revival). refresh_soulprint:
// true → output.refreshed == true (the false stub is removed): the scenario runner
// re-resolves the roster before the next Passage. Catches a hardcoded refreshed:false regression.
func TestApply_RefreshSoulprint_OutputTrue(t *testing.T) {
	fs := newFakeStore()
	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":               "h1.example.com",
			"coven":             []any{"prod"},
			"refresh_soulprint": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := stream.Last().Output.AsMap()
	if out["refreshed"] != true {
		t.Errorf("refreshed=%v, want true (ADR-061 §S3: flag revived)", out["refreshed"])
	}
}

// TestApply_NoRefreshSoulprint_OutputFalse — without refresh_soulprint → refreshed:false
// (behavior before ADR-061 is unchanged: no re-resolve requested).
func TestApply_NoRefreshSoulprint_OutputFalse(t *testing.T) {
	fs := newFakeStore()
	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := stream.Last().Output.AsMap()
	if out["refreshed"] != false {
		t.Errorf("refreshed=%v, want false (no refresh_soulprint)", out["refreshed"])
	}
}

func TestApply_Append_Idempotent_NoChange(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod", "db"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod"},
			"mode":  "append",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("expected changed=false on idempotent append, got %+v", ev)
	}
	if fs.updateCalls != 0 {
		t.Errorf("expected no UpdateCoven calls, got %d", fs.updateCalls)
	}
}

func TestApply_Replace_OverwritesExisting(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod", "db", "old-cluster"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod", "new-cluster"},
			"mode":  "replace",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}
	got := fs.lastCoven
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"new-cluster", "prod"}) {
		t.Errorf("saved coven = %v, want [new-cluster prod]", got)
	}
}

func TestApply_Replace_EmptyCoven_ClearsTags(t *testing.T) {
	// NIM-124: the former "replace requires non-empty coven" footgun guard is
	// removed — membership is a separate relation, so replace+empty coven simply
	// clears the stable tags (it can no longer sever a host from its incarnation).
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{},
			"mode":  "replace",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("replace+empty coven must succeed now (NIM-124), got failed: %+v", ev)
	}
	if len(fs.lastCoven) != 0 {
		t.Errorf("saved coven = %v, want [] (cleared)", fs.lastCoven)
	}
}

func TestApply_Remove_StripsLabels(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod", "db", "old"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"old", "missing"},
			"mode":  "remove",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}
	out := ev.Output.AsMap()
	removed, _ := out["removed"].([]any)
	if !reflect.DeepEqual(removed, []any{"old"}) {
		t.Errorf("removed=%v, want [old]", removed)
	}
	saved := fs.lastCoven
	sort.Strings(saved)
	if !reflect.DeepEqual(saved, []string{"db", "prod"}) {
		t.Errorf("saved coven = %v, want [db prod]", saved)
	}
}

func TestApply_Remove_NoOpWhenNothingMatches(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"nonexistent"},
			"mode":  "remove",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("expected no-op (changed=false), got %+v", ev)
	}
	if fs.updateCalls != 0 {
		t.Errorf("UpdateCoven called on no-op")
	}
}

func TestApply_InvalidSID(t *testing.T) {
	m := coremodsoul.New(newFakeStore())
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "BAD.HOST",
			"coven": []any{"prod"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on invalid sid")
	}
}

func TestApply_InvalidCoven(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{"prod"}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod", "a_b"}, // a_b is not kebab-case
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on invalid coven label")
	}
	if fs.updateCalls != 0 {
		t.Errorf("UpdateCoven should not be called on invalid coven")
	}
}

func TestApply_ValidKebabCoven(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h1.example.com"] = &keepersoul.Soul{SID: "h1.example.com", Coven: []string{}}

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h1.example.com",
			"coven": []any{"prod", "eu-west-1"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("valid kebab-case coven should pass, got %+v", stream.Last())
	}
}

func TestApply_UpdateError_Propagated(t *testing.T) {
	fs := newFakeStore()
	fs.byID["h.example.com"] = &keepersoul.Soul{SID: "h.example.com", Coven: []string{"a"}}
	fs.updateErr = errors.New("boom")

	m := coremodsoul.New(fs)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"sid":   "h.example.com",
			"coven": []any{"b"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatalf("expected failed=true on update error, got %+v", ev)
	}
}
