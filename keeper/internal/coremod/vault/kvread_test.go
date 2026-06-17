package vault_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeVault struct {
	resp map[string]any
	err  error
	last string
}

func (f *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.last = path
	return f.resp, f.err
}

type fakeAudit struct {
	events []*audit.Event
	err    error
}

func (a *fakeAudit) Write(_ context.Context, e *audit.Event) error {
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, e)
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// kvV2 строит KV v2 ответ ({"data": {...}, "metadata": {...}}) как его отдаёт
// hashicorp/vault api.
func kvV2(data map[string]any) map[string]any {
	return map[string]any{"data": data, "metadata": map[string]any{"version": float64(1)}}
}

func TestValidate_RequiresPathAndState(t *testing.T) {
	m := coremodvault.New(&fakeVault{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected errors")
	}
	if len(rep.Errors) < 2 {
		t.Fatalf("expected ≥2 errors, got %v", rep.Errors)
	}
}

func TestApply_ReadKVv2_AllFields(t *testing.T) {
	fv := &fakeVault{resp: kvV2(map[string]any{
		"username": "admin",
		"password": "s3cret",
	})}
	fa := &fakeAudit{}
	m := coremodvault.New(fv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-read",
		Params: mustStruct(t, map[string]any{
			"path": "secret/redis/admin",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || ev.Changed {
		t.Fatalf("expected changed=false failed=false, got %+v", ev)
	}
	if fv.last != "secret/redis/admin" {
		t.Errorf("path passed to Vault = %q", fv.last)
	}
	out := ev.Output.AsMap()
	data := out["data"].(map[string]any)
	if data["password"] != "s3cret" || data["username"] != "admin" {
		t.Errorf("data=%v", data)
	}
	fields := out["fields"].([]any)
	gotFields := make([]string, len(fields))
	for i, f := range fields {
		gotFields[i] = f.(string)
	}
	sort.Strings(gotFields)
	if !reflect.DeepEqual(gotFields, []string{"password", "username"}) {
		t.Errorf("fields=%v", gotFields)
	}
	if len(fa.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(fa.events))
	}
	got := fa.events[0]
	if got.EventType != audit.EventVaultKVRead || got.Source != audit.SourceKeeperInternal {
		t.Errorf("audit event = %+v", got)
	}
	// Сами секреты в audit-payload не должны попасть.
	if _, has := got.Payload["password"]; has {
		t.Error("audit payload contains password — sensitive leak")
	}
	if _, has := got.Payload["data"]; has {
		t.Error("audit payload contains data — sensitive leak")
	}
}

func TestApply_FieldsFilter(t *testing.T) {
	fv := &fakeVault{resp: kvV2(map[string]any{
		"username": "admin",
		"password": "s3cret",
		"extra":    "noise",
	})}
	m := coremodvault.New(fv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-read",
		Params: mustStruct(t, map[string]any{
			"path":   "secret/redis/admin",
			"fields": []any{"password"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := stream.Last().Output.AsMap()
	data := out["data"].(map[string]any)
	if _, has := data["username"]; has {
		t.Error("data leaked username outside requested fields")
	}
	if data["password"] != "s3cret" {
		t.Errorf("data=%v", data)
	}
}

func TestApply_VaultError(t *testing.T) {
	fv := &fakeVault{err: errors.New("forbidden")}
	m := coremodvault.New(fv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-read",
		Params: mustStruct(t, map[string]any{"path": "secret/x"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on vault error")
	}
}

func TestApply_AuditWriteError_FailsTask(t *testing.T) {
	fv := &fakeVault{resp: kvV2(map[string]any{"k": "v"})}
	fa := &fakeAudit{err: errors.New("pg down")}
	m := coremodvault.New(fv, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-read",
		Params: mustStruct(t, map[string]any{"path": "secret/x"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on audit write error (compliance must not be silently skipped)")
	}
}

func TestApply_NilAudit_OK(t *testing.T) {
	fv := &fakeVault{resp: kvV2(map[string]any{"k": "v"})}
	m := coremodvault.New(fv, nil)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-read",
		Params: mustStruct(t, map[string]any{"path": "secret/x"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failed: %+v", ev)
	}
}

func TestApply_KVv1_Fallback(t *testing.T) {
	// KV v1 — без обёртки data/metadata.
	fv := &fakeVault{resp: map[string]any{"k": "v"}}
	m := coremodvault.New(fv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "kv-read",
		Params: mustStruct(t, map[string]any{"path": "kv-v1/x"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failed: %+v", ev)
	}
	data := ev.Output.AsMap()["data"].(map[string]any)
	if data["k"] != "v" {
		t.Errorf("data=%v", data)
	}
}
