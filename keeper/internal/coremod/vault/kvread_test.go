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

type writeCall struct {
	path string
	data map[string]any
}

type fakeVault struct {
	resp  map[string]any
	err   error
	last  string
	wrote []writeCall
}

func (f *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.last = path
	return f.resp, f.err
}

// WriteKV is not called by kv-read (read-state), but Module.Vault now requires
// full VaultWriter (shared with kv-present). Stub captures call fact so
// read-tests catch regression if read-branch accidentally starts writing.
func (f *fakeVault) WriteKV(_ context.Context, path string, data map[string]any) error {
	f.wrote = append(f.wrote, writeCall{path: path, data: data})
	return nil
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

// flatSecret is what vault.Client.ReadKV actually returns: flat payload
// (secret fields), without `{data,metadata}` wrapper. KV version resolved
// transparently inside Client (ADR-017(b) amendment 2026-06-22), module doesn't see it.
func flatSecret(data map[string]any) map[string]any {
	return data
}

// TestValidate_UnknownState checks unknown state routed to dispatcher default-branch
// and yields exactly one error (pattern in cloud/choir: state checked
// first, before specific state's parameters).
func TestValidate_UnknownState(t *testing.T) {
	m := coremodvault.New(&fakeVault{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "fetch",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected error for unknown state")
	}
}

// TestValidate_ReadRequiresPath checks that on valid kv-read state,
// absence of required `path` yields error.
func TestValidate_ReadRequiresPath(t *testing.T) {
	m := coremodvault.New(&fakeVault{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "kv-read",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected error: kv-read requires path")
	}
}

func TestApply_ReadKVv2_AllFields(t *testing.T) {
	fv := &fakeVault{resp: flatSecret(map[string]any{
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
	// Secrets themselves must not appear in audit-payload.
	if _, has := got.Payload["password"]; has {
		t.Error("audit payload contains password — sensitive leak")
	}
	if _, has := got.Payload["data"]; has {
		t.Error("audit payload contains data — sensitive leak")
	}
}

func TestApply_FieldsFilter(t *testing.T) {
	fv := &fakeVault{resp: flatSecret(map[string]any{
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

// TestApply_FieldsFilter_MissingRequestedKey checks that field requested in `fields`
// but absent in secret is silently skipped (no failure, doesn't appear in output):
// caller wanted optional fields, audit-event already spent on secret read.
// Present ones from requested are returned.
func TestApply_FieldsFilter_MissingRequestedKey(t *testing.T) {
	fv := &fakeVault{resp: flatSecret(map[string]any{
		"password": "s3cret",
	})}
	m := coremodvault.New(fv, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "kv-read",
		Params: mustStruct(t, map[string]any{
			"path":   "secret/redis/admin",
			"fields": []any{"password", "absent"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("missing requested field must not fail task, got %+v", ev)
	}
	out := ev.Output.AsMap()
	data := out["data"].(map[string]any)
	if _, has := data["absent"]; has {
		t.Error("absent requested field must not appear in data")
	}
	if data["password"] != "s3cret" {
		t.Errorf("present requested field dropped: data=%v", data)
	}
	fields := out["fields"].([]any)
	if len(fields) != 1 || fields[0].(string) != "password" {
		t.Errorf("fields output should list only present keys, got %v", fields)
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
	fv := &fakeVault{resp: flatSecret(map[string]any{"k": "v"})}
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
	fv := &fakeVault{resp: flatSecret(map[string]any{"k": "v"})}
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

func TestApply_PlainPayload_PassedThrough(t *testing.T) {
	// ReadKV always returns flat payload (v1 and v2 same — version resolved in vault.Client).
	// Module puts it in data as-is, without unwrapping wrapper. Previous extractKVData-fallback
	// removed as latent bug (wrapper {data,metadata} never was in ReadKV).
	fv := &fakeVault{resp: flatSecret(map[string]any{"k": "v"})}
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
