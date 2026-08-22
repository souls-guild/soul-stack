package state_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodstate "github.com/souls-guild/soul-stack/keeper/internal/coremod/state"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/secretpolicy"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeVault is an in-memory KV: ReadKV returns the stored payload (or
// ErrVaultKVNotFound), WriteKV replaces it. writes counts the calls, so a test can
// assert that a SECOND run wrote nothing — the present-vs-set distinction is
// invisible in the returned reference and visible only here.
type fakeVault struct {
	store    map[string]map[string]any
	readErr  error
	writeErr error
	writes   int
}

func newFakeVault(seed map[string]map[string]any) *fakeVault {
	store := map[string]map[string]any{}
	for p, m := range seed {
		cp := map[string]any{}
		for k, v := range m {
			cp[k] = v
		}
		store[p] = cp
	}
	return &fakeVault{store: store}
}

func (v *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if v.readErr != nil {
		return nil, v.readErr
	}
	m, ok := v.store[path]
	if !ok {
		return nil, keepervault.ErrVaultKVNotFound
	}
	cp := map[string]any{}
	for k, val := range m {
		cp[k] = val
	}
	return cp, nil
}

func (v *fakeVault) WriteKV(_ context.Context, path string, data map[string]any) error {
	v.writes++
	if v.writeErr != nil {
		return v.writeErr
	}
	cp := map[string]any{}
	for k, val := range data {
		cp[k] = val
	}
	v.store[path] = cp
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

// collectionSchema is the wb-service-redis shape: a list of users, each with a
// `password` declared `type: secret` and addressed by its `name` sibling.
func collectionSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			"redis_users": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"perms":    map[string]any{"type": "string"},
						"password": map[string]any{"type": "secret", "key": "name"},
					},
				},
			},
			"port": map[string]any{"type": "integer"},
		},
	}
}

// scalarSchema is the one-value-per-incarnation shape.
func scalarSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			"admin_password": map[string]any{"type": "secret"},
		},
	}
}

func runScope(schema map[string]any) context.Context {
	ctx := coremodutil.WithService(context.Background(), "wb-service-redis")
	ctx = coremodutil.WithIncarnation(ctx, "redis-prod")
	return coremodutil.WithStateSchema(ctx, schema)
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// apply runs one Apply and returns the final event.
func apply(t *testing.T, m *coremodstate.Module, ctx context.Context, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	st := internaltest.NewApplyStreamCtx(ctx)
	req := &pluginv1.ApplyRequest{State: coremodstate.StatePresent, Params: mustStruct(t, params)}
	if err := m.Apply(req, st); err != nil {
		t.Fatalf("Apply: unexpected gRPC error %v", err)
	}
	last := st.Last()
	if last == nil {
		t.Fatal("Apply: no final event")
	}
	return last
}

// marker is the travelling form of a `generate_secret({…})` call.
func marker(t *testing.T, policy map[string]any) map[string]any {
	t.Helper()
	p, err := secretpolicy.Parse(policy, secretpolicy.Default())
	if err != nil {
		t.Fatalf("secretpolicy.Parse: %v", err)
	}
	return secretpolicy.Marker(p)
}

func effectiveList(t *testing.T, ev *pluginv1.ApplyEvent) []any {
	t.Helper()
	out := ev.GetOutput().AsMap()
	list, ok := out[coremodstate.OutputEffective].([]any)
	if !ok {
		t.Fatalf("output.effective: want a list, got %T (%v)", out[coremodstate.OutputEffective], out)
	}
	return list
}

// TestPresent_Collection_MintsAbsent: an element whose secret has no value yet is
// minted, and what comes back is a REFERENCE — the value is in Vault and nowhere
// in the register.
func TestPresent_Collection_MintsAbsent(t *testing.T) {
	fv, fa := newFakeVault(nil), &fakeAudit{}
	m := coremodstate.New(fv, fa, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{
			map[string]any{"name": "alice", "perms": "+@read", "password": marker(t, map[string]any{"length": 40})},
		},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false, want true (a secret was minted)")
	}

	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	stored, _ := fv.store[path]["password"].(string)
	if len(stored) != 40 {
		t.Fatalf("vault %s#password: length %d, want 40", path, len(stored))
	}

	elem := effectiveList(t, ev)[0].(map[string]any)
	if got, want := elem["password"], "vault:"+path+"#password"; got != want {
		t.Errorf("effective[0].password = %v, want %v", got, want)
	}
	if elem["perms"] != "+@read" {
		t.Errorf("effective[0].perms = %v, want the non-secret property unchanged", elem["perms"])
	}
	if strings.Contains(ev.String(), stored) {
		t.Error("the minted value appears in the ApplyEvent -- the register must carry a reference only")
	}
	if len(fa.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(fa.events))
	}
	if strings.Contains(payloadText(t, fa.events[0]), stored) {
		t.Error("the minted value appears in the audit payload")
	}
}

// TestPresent_Collection_KeepsExisting: the second run of the same scenario must
// not rotate a live credential. `present`, not `set`.
func TestPresent_Collection_KeepsExisting(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "the-first-run-value"}})
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice", "perms": "+@read", "password": marker(t, map[string]any{})}},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true, want false: an existing secret is kept")
	}
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0", fv.writes)
	}
	if got := fv.store[path]["password"]; got != "the-first-run-value" {
		t.Errorf("stored password = %v, want the first run's value", got)
	}
	if gen := ev.GetOutput().AsMap()[coremodstate.OutputGenerated].([]any); len(gen) != 0 {
		t.Errorf("generated = %v, want empty", gen)
	}
}

// TestPresent_Collection_MergesNeighbouringField: minting one field must not drop
// another field of the same KV entry.
func TestPresent_Collection_MergesNeighbouringField(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"note": "keep-me"}})
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice", "password": marker(t, map[string]any{})}},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if got := fv.store[path]["note"]; got != "keep-me" {
		t.Errorf("neighbouring field note = %v, want keep-me", got)
	}
	if fv.store[path]["password"] == nil {
		t.Error("password was not written")
	}
}

// TestPresent_Scalar_MintsAndReferences: a field that IS one secret has no key
// segment and lands under the ADR's scalar field name.
func TestPresent_Scalar_MintsAndReferences(t *testing.T) {
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"key": "admin_password",
		"set": marker(t, map[string]any{"charset": "hex", "length": 16}),
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	const path = "secret/wb-service-redis/redis-prod/admin_password"
	stored, _ := fv.store[path]["value"].(string)
	if len(stored) != 16 || strings.Trim(stored, "0123456789abcdef") != "" {
		t.Fatalf("stored value %q does not match the requested policy", stored)
	}
	if got := ev.GetOutput().AsMap()[coremodstate.OutputEffective]; got != "vault:"+path+"#value" {
		t.Errorf("effective = %v, want the scalar reference", got)
	}
}

// TestPresent_EmptyStringCountsAsAbsent: an empty stored value must be mintable,
// or a botched first write would leave the field permanently empty.
func TestPresent_EmptyStringCountsAsAbsent(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/admin_password"
	fv := newFakeVault(map[string]map[string]any{path: {"value": ""}})
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"key": "admin_password", "set": marker(t, map[string]any{}),
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false: an empty stored value must count as absent")
	}
}

// TestPresent_RejectsPlaintextOnSecretProperty: a literal password in a task param
// has already travelled through render and the run's diagnostics. Refusing it is
// the point of the ADR.
func TestPresent_RejectsPlaintextOnSecretProperty(t *testing.T) {
	// The vault is SEEDED at the derived path, so the refusal is exercised on the
	// update branch rather than the create one. An empty store sends the module down
	// the "absent" arm, where a write is a fresh secret; the arm that already holds a
	// value is the one where a literal could plausibly be taken as the new content.
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "minted-earlier"}})
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice", "password": "hunter2"}},
	})
	if !ev.GetFailed() {
		t.Fatal("a literal secret was accepted")
	}
	if strings.Contains(ev.GetMessage(), "hunter2") {
		t.Errorf("the rejected value leaked into the message: %s", ev.GetMessage())
	}
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0", fv.writes)
	}
	// The write counter alone would pass an implementation that mutated the store by
	// another route; assert on the CONTENT the store ends up holding.
	for p, entry := range fv.store {
		for field, v := range entry {
			if s, _ := v.(string); s == "hunter2" {
				t.Errorf("the literal reached Vault at %s#%s", p, field)
			}
		}
	}
	if got := fv.store[path]["password"]; got != "minted-earlier" {
		t.Errorf("the existing secret was disturbed by a refused apply: %v", got)
	}
}

// TestPresent_RejectsMarkerOnNonSecretProperty: a request where nothing is declared
// would otherwise land in `incarnation.state` as an ordinary map and look like a
// password that was never minted.
func TestPresent_RejectsMarkerOnNonSecretProperty(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice", "perms": marker(t, map[string]any{})}},
	})
	if !ev.GetFailed() {
		t.Fatal("a secret request on a non-secret property was accepted")
	}
	if !strings.Contains(ev.GetMessage(), "[0].perms") {
		t.Errorf("message does not name the offending position: %s", ev.GetMessage())
	}
}

// TestPresent_RejectsUnsafeKey: `key` is the one path segment that comes from state
// DATA. A traversal attempt must fail closed, not normalise.
func TestPresent_RejectsUnsafeKey(t *testing.T) {
	for _, key := range []string{"../../keeper", "..", ".", "a/b", "", "ali ce"} {
		t.Run(key, func(t *testing.T) {
			fv := newFakeVault(nil)
			m := coremodstate.New(fv, nil, "secret")
			ev := apply(t, m, runScope(collectionSchema()), map[string]any{
				"key": "redis_users",
				"set": []any{map[string]any{"name": key, "password": marker(t, map[string]any{})}},
			})
			if !ev.GetFailed() {
				t.Fatalf("key %q was accepted", key)
			}
			if fv.writes != 0 {
				t.Errorf("WriteKV calls = %d, want 0", fv.writes)
			}
		})
	}
}

// TestPresent_RequiresRunScope: without the run's owner the derived path would have
// an empty segment. Fail rather than build one.
func TestPresent_RequiresRunScope(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ctx := coremodutil.WithStateSchema(context.Background(), collectionSchema())

	ev := apply(t, m, ctx, map[string]any{"key": "redis_users", "set": []any{}})
	if !ev.GetFailed() {
		t.Fatal("the module ran without a service and incarnation")
	}
}

// TestPresent_RequiresStateSchema: the declaration is what makes a property secret.
// No schema means the module cannot tell, and guessing would write plaintext.
func TestPresent_RequiresStateSchema(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ctx := coremodutil.WithIncarnation(coremodutil.WithService(context.Background(), "svc"), "inc")

	ev := apply(t, m, ctx, map[string]any{"key": "redis_users", "set": []any{}})
	if !ev.GetFailed() {
		t.Fatal("the module ran without a state_schema")
	}
}

// TestPresent_RejectsUnknownStateField: a typo in `key` would otherwise be caught
// only by the state commit, one phase later.
func TestPresent_RejectsUnknownStateField(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"key": "redis_userz", "set": []any{}})
	if !ev.GetFailed() {
		t.Fatal("an undeclared state field was accepted")
	}
}

// TestPresent_NonSecretFieldPassesThrough: the module is the write point for a
// state field with no secrets too; the value travels unchanged.
func TestPresent_NonSecretFieldPassesThrough(t *testing.T) {
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"key": "port", "set": float64(6379)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true, want false: nothing was minted")
	}
	if got := ev.GetOutput().AsMap()[coremodstate.OutputEffective]; got != float64(6379) {
		t.Errorf("effective = %v, want 6379", got)
	}
}

// TestPresent_RejectsStrayMarker: a request in a position nobody declared is an
// error, not data. Otherwise the marker map itself would land in state.
func TestPresent_RejectsStrayMarker(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "port",
		"set": map[string]any{"nested": marker(t, map[string]any{})},
	})
	if !ev.GetFailed() {
		t.Fatal("a stray secret request was accepted")
	}
	if !strings.Contains(ev.GetMessage(), ".nested") {
		t.Errorf("message does not name the offending position: %s", ev.GetMessage())
	}
}

// TestPresent_MissingValueWithoutRequest: an empty secret property that asks for
// nothing has no reference to hand back.
func TestPresent_MissingValueWithoutRequest(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice"}},
	})
	if !ev.GetFailed() {
		t.Fatal("a secret property with no value and no request was accepted")
	}
}

// TestPresent_MissingValueWithExisting: the same property IS satisfiable once a
// value exists — a scenario that only reads must not have to re-request.
func TestPresent_MissingValueWithExisting(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "already-there"}})
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{map[string]any{"name": "alice"}},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	elem := effectiveList(t, ev)[0].(map[string]any)
	if got, want := elem["password"], "vault:"+path+"#password"; got != want {
		t.Errorf("effective[0].password = %v, want %v", got, want)
	}
}

// TestPresent_VaultErrorFailsTask: a transport error is a failed task, never a
// silently skipped secret.
func TestPresent_VaultErrorFailsTask(t *testing.T) {
	fv := newFakeVault(nil)
	fv.readErr = errors.New("vault: connection refused")
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"key": "admin_password", "set": marker(t, map[string]any{}),
	})
	if !ev.GetFailed() {
		t.Fatal("a Vault read error did not fail the task")
	}
}

// TestPresent_WriteErrorDoesNotLeakValue: the write path must not quote the value
// it failed to store.
func TestPresent_WriteErrorDoesNotLeakValue(t *testing.T) {
	fv := newFakeVault(nil)
	fv.writeErr = errors.New("vault: permission denied")
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"key": "admin_password", "set": marker(t, map[string]any{"charset": "hex"}),
	})
	if !ev.GetFailed() {
		t.Fatal("a Vault write error did not fail the task")
	}
	if !strings.Contains(ev.GetMessage(), "permission denied") {
		t.Errorf("message lost the cause: %s", ev.GetMessage())
	}
}

// TestPresent_ValidateRejectsUnknownState guards the author form offline.
func TestPresent_ValidateRejectsUnknownState(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"key": "redis_users", "set": "x"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.GetOk() {
		t.Fatal("state `absent` was accepted")
	}
}

// TestPresent_ValidateRequiresParams: `key` and `set` are both required.
func TestPresent_ValidateRequiresParams(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodstate.StatePresent, Params: mustStruct(t, map[string]any{}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.GetOk() || len(rep.GetErrors()) != 2 {
		t.Fatalf("Validate errors = %v, want one per missing param", rep.GetErrors())
	}
}

// TestPresent_GeneratedPathsAreSorted keeps the output deterministic: the same
// input must produce byte-identical events run to run.
func TestPresent_GeneratedPathsAreSorted(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret")
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"key": "redis_users",
		"set": []any{
			map[string]any{"name": "zoe", "password": marker(t, map[string]any{})},
			map[string]any{"name": "alice", "password": marker(t, map[string]any{})},
		},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	gen := ev.GetOutput().AsMap()[coremodstate.OutputGenerated].([]any)
	want := []string{
		"secret/wb-service-redis/redis-prod/redis_users/alice#password",
		"secret/wb-service-redis/redis-prod/redis_users/zoe#password",
	}
	for i, w := range want {
		if gen[i] != w {
			t.Errorf("generated[%d] = %v, want %v", i, gen[i], w)
		}
	}
}

// payloadText flattens an audit payload for a leak check.
func payloadText(t *testing.T, e *audit.Event) string {
	t.Helper()
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, sub := range x {
				walk(sub)
			}
		case []any:
			for _, sub := range x {
				walk(sub)
			}
		case []string:
			for _, sub := range x {
				b.WriteString(sub)
			}
		case string:
			b.WriteString(x)
		}
		b.WriteString("\x00")
	}
	walk(e.Payload)
	return b.String()
}
