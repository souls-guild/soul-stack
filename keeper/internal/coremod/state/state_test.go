package state_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodstate "github.com/souls-guild/soul-stack/keeper/internal/coremod/state"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keeperincarnation "github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/stateop"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
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
func collectionSchema() config.InputSchemaMap {
	return config.InputSchemaMap{
		"redis_users": {
			Type: "array",
			Items: &config.InputSchema{
				Type: "object",
				Properties: config.InputSchemaMap{
					"name":     {Type: "string"},
					"perms":    {Type: "string"},
					"password": {Type: config.SecretTypeName, Key: "name"},
				},
			},
		},
		"port": {Type: "integer"},
	}
}

// scalarSchema is the one-value-per-incarnation shape.
func scalarSchema() config.InputSchemaMap {
	return config.InputSchemaMap{
		"admin_password": {Type: config.SecretTypeName},
	}
}

func runScope(schema config.InputSchemaMap) context.Context {
	ctx := coremodutil.WithService(context.Background(), "wb-service-redis")
	ctx = coremodutil.WithIncarnation(ctx, "redis-prod")
	ctx = coremodutil.WithStateSchema(ctx, schema)
	return coremodutil.WithRunScope(ctx, coremodutil.RunScope{
		Scenario:     "update_users",
		ApplyID:      "01APPLY",
		StartedByAID: "archon-alice",
	})
}

// fakeStore is an in-memory `incarnation.state`: CaptureState runs the mutation
// against what is stored and keeps the result, so a test can assert what LANDED
// in state — which is a different question from what came back in the register,
// and the one [ADR-0084] moved into this module.
type fakeStore struct {
	state map[string]any
	specs []keeperincarnation.CaptureSpec
	err   error
}

func newFakeStore() *fakeStore { return &fakeStore{state: map[string]any{}} }

func (s *fakeStore) CaptureState(_ context.Context, spec keeperincarnation.CaptureSpec, mutate func(map[string]any) (map[string]any, error)) (map[string]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	after, err := mutate(s.state)
	if err != nil {
		return nil, err
	}
	s.state = after
	s.specs = append(s.specs, spec)
	return after, nil
}

func (s *fakeStore) ReadState(_ context.Context, _ string) (map[string]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.state, nil
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
	return applyState(t, m, stateop.StateSet, ctx, params)
}

// applyState runs one Apply of the named state and returns the final event.
func applyState(t *testing.T, m *coremodstate.Module, state string, ctx context.Context, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	st := internaltest.NewApplyStreamCtx(ctx)
	req := &pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}
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
func TestSecret_Collection_MintsAbsent(t *testing.T) {
	fv, fa := newFakeVault(nil), &fakeAudit{}
	m := coremodstate.New(fv, fa, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{
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
// not rotate a live credential. `present`, not `set`. The store is seeded with
// what a first run would have left, so the run is a true repeat and `changed`
// answers for the state write as well as for the mint.
func TestSecret_Collection_KeepsExisting(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "the-first-run-value"}})
	fs := newFakeStore()
	fs.state = map[string]any{"redis_users": []any{map[string]any{"name": "alice", "perms": "+@read"}}}
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "perms": "+@read", "password": marker(t, map[string]any{})}},
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
func TestSecret_Collection_MergesNeighbouringField(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"note": "keep-me"}})
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "password": marker(t, map[string]any{})}},
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
func TestSecret_Scalar_MintsAndReferences(t *testing.T) {
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"field": "admin_password",
		"value": marker(t, map[string]any{"charset": "hex", "length": 16}),
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
func TestSecret_EmptyStringCountsAsAbsent(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/admin_password"
	fv := newFakeVault(map[string]map[string]any{path: {"value": ""}})
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"field": "admin_password", "value": marker(t, map[string]any{}),
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
func TestSecret_RejectsPlaintextOnSecretProperty(t *testing.T) {
	// The vault is SEEDED at the derived path, so the refusal is exercised on the
	// update branch rather than the create one. An empty store sends the module down
	// the "absent" arm, where a write is a fresh secret; the arm that already holds a
	// value is the one where a literal could plausibly be taken as the new content.
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "minted-earlier"}})
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "password": "hunter2"}},
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
func TestSecret_RejectsMarkerOnNonSecretProperty(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "perms": marker(t, map[string]any{})}},
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
func TestSecret_RejectsUnsafeKey(t *testing.T) {
	for _, key := range []string{"../../keeper", "..", ".", "a/b", "", "ali ce"} {
		t.Run(key, func(t *testing.T) {
			fv := newFakeVault(nil)
			m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())
			ev := apply(t, m, runScope(collectionSchema()), map[string]any{
				"field": "redis_users",
				"value": []any{map[string]any{"name": key, "password": marker(t, map[string]any{})}},
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

// TestSecret_Collection_RefusalOnALaterElementWritesNothing: the collection is
// accepted or refused WHOLE (NIM-705).
//
// Every refusal below is decidable from the element alone, so deciding it while
// already minting used to leave the earlier elements' secrets live in Vault with
// nothing referencing them — the run aborts before the capture, so no state row and
// no operator ever learns the paths exist. The offending element is deliberately the
// LAST one: with the refusal made per element, elements 0 and 1 are already written
// by the time it is reached.
func TestSecret_Collection_RefusalOnALaterElementWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		last map[string]any
	}{
		{"unsafe key", map[string]any{"name": "../../keeper", "password": marker(t, map[string]any{})}},
		{"missing key", map[string]any{"perms": "+@read", "password": marker(t, map[string]any{})}},
		{"non-string key", map[string]any{"name": 7, "password": marker(t, map[string]any{})}},
		{"literal secret", map[string]any{"name": "carol", "password": "hunter2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fv := newFakeVault(nil)
			fs := newFakeStore()
			m := coremodstate.New(fv, nil, "secret").WithStore(fs)

			ev := apply(t, m, runScope(collectionSchema()), map[string]any{
				"field": "redis_users",
				"value": []any{
					map[string]any{"name": "alice", "password": marker(t, map[string]any{})},
					map[string]any{"name": "bob", "password": marker(t, map[string]any{})},
					tc.last,
				},
			})
			if !ev.GetFailed() {
				t.Fatal("the collection was accepted")
			}
			if fv.writes != 0 {
				t.Errorf("WriteKV calls = %d, want 0 -- elements before the bad one were minted", fv.writes)
			}
			// The counter alone would pass an implementation mutating the store by
			// another route; assert on what the store actually holds.
			if len(fv.store) != 0 {
				t.Errorf("vault holds %v, want nothing", fv.store)
			}
			if len(fs.state) != 0 {
				t.Errorf("state = %v, want nothing captured", fs.state)
			}
		})
	}
}

// TestSecret_Collection_UnmintableElementWritesNothing: a refusal only a Vault READ
// can make must also leave the collection unwritten (NIM-705).
//
// Element [1] asks for nothing and has nothing stored, so it cannot be referenced --
// the same refusal a scalar gets. Deciding that while walking the elements one by
// one means element [0] is already live in Vault when the run dies, before the
// capture: the orphan this ticket is about, reached by a route no amount of
// value-level validation can foresee.
func TestSecret_Collection_UnmintableElementWritesNothing(t *testing.T) {
	fv := newFakeVault(nil)
	fs := newFakeStore()
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{
			map[string]any{"name": "alice", "password": marker(t, map[string]any{})},
			map[string]any{"name": "bob", "perms": "+@read"},
		},
	})
	if !ev.GetFailed() {
		t.Fatalf("the collection was accepted: %v", effectiveList(t, ev))
	}
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0 -- alice was minted before bob was refused", fv.writes)
	}
	if len(fv.store) != 0 {
		t.Errorf("vault holds %v, want nothing", fv.store)
	}
	if len(fs.state) != 0 {
		t.Errorf("state = %v, want nothing captured", fs.state)
	}
}

// TestSecret_Collection_RefusesDuplicateKeys: two elements addressing one declared
// secret with the same key derive the SAME Vault path (NIM-704).
//
// Left alone the run succeeds and looks right: the first element mints, the second
// finds a value already there and keeps it (present-semantics), and both come back
// holding the same reference. Two accounts on one credential, and neither the state,
// the register nor the audit event says so. So the assertion is that the run FAILS
// and names both positions -- not that the second element minted a second time.
func TestSecret_Collection_RefusesDuplicateKeys(t *testing.T) {
	fv := newFakeVault(nil)
	fs := newFakeStore()
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{
			map[string]any{"name": "alice", "perms": "+@read", "password": marker(t, map[string]any{})},
			map[string]any{"name": "bob", "password": marker(t, map[string]any{})},
			map[string]any{"name": "alice", "perms": "+@write", "password": marker(t, map[string]any{})},
		},
	})
	if !ev.GetFailed() {
		t.Fatalf("the collection was accepted: %v", effectiveList(t, ev))
	}
	for _, want := range []string{"[2]", "[0]", "alice"} {
		if !strings.Contains(ev.GetMessage(), want) {
			t.Errorf("message does not name %q: %s", want, ev.GetMessage())
		}
	}
	// The refusal is made before the first write, so even the elements that were
	// fine are not in Vault -- the run failed, so it wrote nothing (NIM-705).
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0", fv.writes)
	}
	if len(fv.store) != 0 {
		t.Errorf("vault holds %v, want nothing", fv.store)
	}
	if len(fs.state) != 0 {
		t.Errorf("state = %v, want nothing captured", fs.state)
	}
}

// TestSecret_Collection_DuplicateKeyIsPerDeclaredSecret: what must be unique is the
// (key, property) pair the path is derived from, not the element's identity.
//
// Two properties of one element may be addressed by DIFFERENT `key:` siblings, and
// then the same text legitimately appears as a key twice -- once per declared
// secret, deriving two different paths. A check keyed on the element rather than on
// the declared secret would refuse this.
func TestSecret_Collection_DuplicateKeyIsPerDeclaredSecret(t *testing.T) {
	schema := config.InputSchemaMap{
		"accounts": {
			Type: "array",
			Items: &config.InputSchema{
				Type: "object",
				Properties: config.InputSchemaMap{
					"name":     {Type: "string"},
					"alias":    {Type: "string"},
					"password": {Type: config.SecretTypeName, Key: "name"},
					"token":    {Type: config.SecretTypeName, Key: "alias"},
				},
			},
		},
	}
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(schema), map[string]any{
		"field": "accounts",
		"value": []any{
			map[string]any{"name": "alice", "alias": "ops", "password": marker(t, map[string]any{}), "token": marker(t, map[string]any{})},
			map[string]any{"name": "ops", "alias": "alice", "password": marker(t, map[string]any{}), "token": marker(t, map[string]any{})},
		},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	for _, path := range []string{
		"secret/wb-service-redis/redis-prod/accounts/alice",
		"secret/wb-service-redis/redis-prod/accounts/ops",
	} {
		if len(fv.store[path]) != 2 {
			t.Errorf("vault %s = %v, want both a password and a token", path, fv.store[path])
		}
	}
}

// TestPresent_RequiresRunScope: without the run's owner the derived path would have
// an empty segment. Fail rather than build one.
func TestSecret_RequiresRunScope(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ctx := coremodutil.WithStateSchema(context.Background(), collectionSchema())

	ev := apply(t, m, ctx, map[string]any{"field": "redis_users", "value": []any{}})
	if !ev.GetFailed() {
		t.Fatal("the module ran without a service and incarnation")
	}
}

// TestPresent_RequiresStateSchema: the declaration is what makes a property secret.
// No schema means the module cannot tell, and guessing would write plaintext.
func TestSecret_RequiresStateSchema(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ctx := coremodutil.WithIncarnation(coremodutil.WithService(context.Background(), "svc"), "inc")

	ev := apply(t, m, ctx, map[string]any{"field": "redis_users", "value": []any{}})
	if !ev.GetFailed() {
		t.Fatal("the module ran without a state_schema")
	}
}

// TestPresent_RejectsUnknownStateField: a typo in `key` would otherwise be caught
// only by the state commit, one phase later.
func TestSecret_RejectsUnknownStateField(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"field": "redis_userz", "value": []any{}})
	if !ev.GetFailed() {
		t.Fatal("an undeclared state field was accepted")
	}
}

// TestPresent_NonSecretFieldPassesThrough: the module is the write point for a
// state field with no secrets too; the value travels unchanged.
func TestSecret_NonSecretFieldPassesThrough(t *testing.T) {
	fv := newFakeVault(nil)
	fs := newFakeStore()
	fs.state = map[string]any{"port": float64(6379)}
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"field": "port", "value": float64(6379)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true, want false: nothing was minted and the stored value is the same")
	}
	if got := ev.GetOutput().AsMap()[coremodstate.OutputEffective]; got != float64(6379) {
		t.Errorf("effective = %v, want 6379", got)
	}
}

// TestPresent_RejectsStrayMarker: a request in a position nobody declared is an
// error, not data. Otherwise the marker map itself would land in state.
func TestSecret_RejectsStrayMarker(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "port",
		"value": map[string]any{"nested": marker(t, map[string]any{})},
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
func TestSecret_MissingValueWithoutRequest(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice"}},
	})
	if !ev.GetFailed() {
		t.Fatal("a secret property with no value and no request was accepted")
	}
}

// TestPresent_MissingValueWithExisting: the same property IS satisfiable once a
// value exists — a scenario that only reads must not have to re-request.
func TestSecret_MissingValueWithExisting(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "already-there"}})
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice"}},
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
func TestSecret_VaultErrorFailsTask(t *testing.T) {
	fv := newFakeVault(nil)
	fv.readErr = errors.New("vault: connection refused")
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"field": "admin_password", "value": marker(t, map[string]any{}),
	})
	if !ev.GetFailed() {
		t.Fatal("a Vault read error did not fail the task")
	}
}

// TestPresent_WriteErrorDoesNotLeakValue: the write path must not quote the value
// it failed to store.
func TestSecret_WriteErrorDoesNotLeakValue(t *testing.T) {
	fv := newFakeVault(nil)
	fv.writeErr = errors.New("vault: permission denied")
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ev := apply(t, m, runScope(scalarSchema()), map[string]any{
		"field": "admin_password", "value": marker(t, map[string]any{"charset": "hex"}),
	})
	if !ev.GetFailed() {
		t.Fatal("a Vault write error did not fail the task")
	}
	if !strings.Contains(ev.GetMessage(), "permission denied") {
		t.Errorf("message lost the cause: %s", ev.GetMessage())
	}
}

// TestPresent_ValidateRejectsUnknownState guards the author form offline.
func TestState_ValidateRejectsUnknownState(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"field": "redis_users", "value": "x"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.GetOk() {
		t.Fatal("state `absent` was accepted")
	}
}

// TestPresent_ValidateRequiresParams: `key` and `set` are both required.
func TestState_ValidateRequiresParams(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	rep, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: stateop.StatePresent, Params: mustStruct(t, map[string]any{}),
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
func TestSecret_GeneratedPathsAreSorted(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{
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

// TestCapture_WritesStrippedFieldAtTheStep is the [ADR-0084] move itself: the
// field lands in `incarnation.state` at the step, not in an end-of-run commit,
// and it lands STRIPPED — the value is in Vault, the reference is in the
// register, and neither is in state.
func TestCapture_WritesStrippedFieldAtTheStep(t *testing.T) {
	fv, fs := newFakeVault(nil), newFakeStore()
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{
			map[string]any{"name": "alice", "perms": "+@read", "password": marker(t, map[string]any{"length": 40})},
		},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}

	list, ok := fs.state["redis_users"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("state.redis_users = %v, want a one-element list", fs.state["redis_users"])
	}
	stored := list[0].(map[string]any)
	if _, present := stored["password"]; present {
		t.Errorf("state carries the declared secret property: %v", stored)
	}
	if stored["name"] != "alice" || stored["perms"] != "+@read" {
		t.Errorf("state.redis_users[0] = %v, want the non-secret properties intact", stored)
	}

	// The same value went into the register and into the write. The write strips;
	// the register must not, or the reference the next step reads is gone.
	elem := effectiveList(t, ev)[0].(map[string]any)
	const want = "vault:secret/wb-service-redis/redis-prod/redis_users/alice#password"
	if elem["password"] != want {
		t.Errorf("effective[0].password = %v, want %v", elem["password"], want)
	}
	minted, _ := fv.store["secret/wb-service-redis/redis-prod/redis_users/alice"]["password"].(string)
	if minted == "" {
		t.Fatal("nothing was minted")
	}
	if strings.Contains(fmt.Sprint(fs.state), minted) {
		t.Error("the minted value appears in state -- state carries the reference, Vault carries the value")
	}
}

// TestCapture_NamesTheRunThatCausedIt: the history row has to point at its cause.
// With the end-of-run commit gone, this module is the only writer that can name it.
func TestCapture_NamesTheRunThatCausedIt(t *testing.T) {
	fs := newFakeStore()
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"field": "port", "value": float64(6379)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if len(fs.specs) != 1 {
		t.Fatalf("capture calls = %d, want 1", len(fs.specs))
	}
	spec := fs.specs[0]
	if spec.ID != "redis-prod" || spec.Scenario != "update_users" || spec.ApplyID != "01APPLY" {
		t.Errorf("spec = %+v, want the run's incarnation/scenario/apply_id", spec)
	}
	if spec.ChangedByAID == nil || *spec.ChangedByAID != "archon-alice" {
		t.Errorf("spec.ChangedByAID = %v, want archon-alice", spec.ChangedByAID)
	}
	if spec.HistoryID == "" {
		t.Error("spec.HistoryID is empty -- state_history has it as the primary key")
	}
}

// TestCapture_ChangedWithoutAMint: a field that is written for the first time is
// a change even when no secret was minted. Reporting only the mint would call a
// real state change a no-op.
func TestCapture_ChangedWithoutAMint(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"field": "port", "value": float64(6379)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false, want true: the stored field went from absent to 6379")
	}
}

// TestCapture_NoRunScopeMintsNothing: a step that cannot record what it resolved
// must not resolve it. Otherwise a live credential exists in Vault with nothing
// in state pointing at it.
func TestCapture_NoRunScopeMintsNothing(t *testing.T) {
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret").WithStore(newFakeStore())

	ctx := coremodutil.WithService(context.Background(), "wb-service-redis")
	ctx = coremodutil.WithIncarnation(ctx, "redis-prod")
	ctx = coremodutil.WithStateSchema(ctx, collectionSchema())

	ev := apply(t, m, ctx, map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "password": marker(t, map[string]any{})}},
	})
	if !ev.GetFailed() {
		t.Fatal("a capture without a run scope was accepted")
	}
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0 -- the guard must run before the mint", fv.writes)
	}
}

// TestCapture_NoStoreMintsNothing: same guard, other dependency.
func TestCapture_NoStoreMintsNothing(t *testing.T) {
	fv := newFakeVault(nil)
	m := coremodstate.New(fv, nil, "secret")

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "alice", "password": marker(t, map[string]any{})}},
	})
	if !ev.GetFailed() {
		t.Fatal("a capture without a store was accepted")
	}
	if fv.writes != 0 {
		t.Errorf("WriteKV calls = %d, want 0 -- the guard must run before the mint", fv.writes)
	}
}

// TestCapture_WriteFailureFailsTheStep: a resolved secret nothing recorded is a
// failed step, not a successful one with a silent gap.
func TestCapture_WriteFailureFailsTheStep(t *testing.T) {
	fs := newFakeStore()
	fs.err = errors.New("incarnation: already finalized")
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)

	ev := apply(t, m, runScope(collectionSchema()), map[string]any{"field": "port", "value": float64(6379)})
	if !ev.GetFailed() {
		t.Fatal("a failed capture was reported as success")
	}
	if !strings.Contains(ev.GetMessage(), "already finalized") {
		t.Errorf("message = %q, want the store's cause", ev.GetMessage())
	}
}

// plainSchema is a field with no declared secret: the verb dispatch is what is
// under test, and secret resolution is orthogonal to it.
func plainSchema() config.InputSchemaMap {
	return config.InputSchemaMap{
		"users":  {Type: "array"},
		"conf":   {Type: "object"},
		"port":   {Type: "integer"},
		"events": {Type: "array"},
	}
}

// withEvals attaches merge-time evaluators that match on a literal CEL-free
// predicate `elem.name == "<x>"`, which is the form a rendered task param
// carries by the time the module sees it (`${ … }` was interpolated one phase
// earlier). Real CEL lives in the render pipeline's own tests.
func withEvals(ctx context.Context) context.Context {
	name := func(v any) string {
		m, _ := v.(map[string]any)
		s, _ := m["name"].(string)
		return s
	}
	want := func(expr string) string {
		_, rest, ok := strings.Cut(expr, `== "`)
		if !ok {
			return ""
		}
		return strings.TrimSuffix(rest, `"`)
	}
	return coremodutil.WithStateOpEvaluators(ctx, coremodutil.StateOpEvaluators{
		Match: func(predicate string, elem, value any) (bool, error) {
			if predicate == "" {
				return false, nil
			}
			return name(elem) == name(value), nil
		},
		Op: func(expr string, binds map[string]any, boolOut bool) (any, error) {
			if !boolOut {
				return expr, nil
			}
			return name(binds["elem"]) == want(expr), nil
		},
	})
}

// storeWith seeds the fake state so a verb has something to operate on.
func storeWith(state map[string]any) *fakeStore { return &fakeStore{state: state} }

// TestState_SetOverwrites and TestState_PresentKeepsAnExistingValue are the
// halves of the fork [ADR-0084] settled: the two verbs differ ONLY in what they
// do to a slot that is already full.
func TestState_SetOverwrites(t *testing.T) {
	fs := storeWith(map[string]any{"port": float64(6379)})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StateSet, runScope(plainSchema()),
		map[string]any{"field": "port", "value": float64(6380)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if fs.state["port"] != float64(6380) {
		t.Errorf("* set did not overwrite: state = %#v", fs.state)
	}
	if !ev.GetChanged() {
		t.Error("changed = false, want true")
	}
}

func TestState_PresentKeepsAnExistingValue(t *testing.T) {
	fs := storeWith(map[string]any{"port": float64(6379)})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StatePresent, runScope(plainSchema()),
		map[string]any{"field": "port", "value": float64(6380)})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if fs.state["port"] != float64(6379) {
		t.Errorf("* present overwrote an existing value: state = %#v", fs.state)
	}
	if ev.GetChanged() {
		t.Error("* changed = true on a no-op")
	}
}

func TestState_PresentFillsAnEmptySlot(t *testing.T) {
	for name, before := range map[string]map[string]any{
		"absent": {},
		"null":   {"port": nil},
	} {
		t.Run(name, func(t *testing.T) {
			fs := storeWith(before)
			m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
			ev := applyState(t, m, stateop.StatePresent, runScope(plainSchema()),
				map[string]any{"field": "port", "value": float64(6380)})
			if ev.GetFailed() {
				t.Fatalf("failed: %s", ev.GetMessage())
			}
			if fs.state["port"] != float64(6380) {
				t.Errorf("* present did not fill an empty slot: %#v", fs.state)
			}
		})
	}
}

// TestState_PresentOverAnExistingFieldMintsNothing is the reason `present` reads
// state BEFORE resolving: minting a secret for a write that is then discarded
// would leave a live credential nothing in state points at.
func TestState_PresentOverAnExistingFieldMintsNothing(t *testing.T) {
	const path = "secret/wb-service-redis/redis-prod/redis_users/alice"
	fv := newFakeVault(map[string]map[string]any{path: {"password": "already-there"}})
	fs := storeWith(map[string]any{"redis_users": []any{map[string]any{"name": "alice"}}})
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)

	ev := applyState(t, m, stateop.StatePresent, runScope(collectionSchema()), map[string]any{
		"field": "redis_users",
		"value": []any{map[string]any{"name": "bob", "password": marker(t, map[string]any{})}},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if fv.writes != 0 {
		t.Errorf("* present minted %d secret(s) for a write it discarded", fv.writes)
	}
	if ev.GetChanged() {
		t.Error("* changed = true on a no-op")
	}
	// The register still quotes the STORED element, not the proposed one.
	elem := effectiveList(t, ev)[0].(map[string]any)
	if got, want := elem["password"], "vault:"+path+"#password"; got != want {
		t.Errorf("effective[0].password = %v, want a reference to the stored secret %v", got, want)
	}
}

func TestState_AddIsIdempotent(t *testing.T) {
	fs := storeWith(map[string]any{"users": []any{map[string]any{"name": "alice"}}})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	params := map[string]any{
		"field": "users",
		"value": map[string]any{"name": "alice"},
		"match": `elem.name == value.name`,
	}
	ev := applyState(t, m, stateop.StateAdd, withEvals(runScope(plainSchema())), params)
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if got := fs.state["users"].([]any); len(got) != 1 {
		t.Errorf("* add duplicated a matching element: %#v", got)
	}
	if ev.GetChanged() {
		t.Error("* changed = true on an idempotent no-op")
	}
}

// TestState_AppendDoesNotDedup separates `append` from `add` at the module
// level: the same element twice is two elements, no match predicate involved.
func TestState_AppendDoesNotDedup(t *testing.T) {
	fs := storeWith(map[string]any{})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	params := map[string]any{"field": "events", "value": "restarted"}
	for i := 0; i < 2; i++ {
		if ev := applyState(t, m, stateop.StateAppend, runScope(plainSchema()), params); ev.GetFailed() {
			t.Fatalf("failed: %s", ev.GetMessage())
		}
	}
	if got := fs.state["events"].([]any); len(got) != 2 {
		t.Errorf("* append deduped: %#v", got)
	}
}

func TestState_ModifyPatchesMatchingElements(t *testing.T) {
	fs := storeWith(map[string]any{"users": []any{
		map[string]any{"name": "alice", "perms": "+@read"},
		map[string]any{"name": "bob", "perms": "+@read"},
	}})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StateModify, withEvals(runScope(plainSchema())), map[string]any{
		"field": "users",
		"match": `elem.name == "bob"`,
		"patch": map[string]any{"perms": "+@all"},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	users := fs.state["users"].([]any)
	if got := users[0].(map[string]any)["perms"]; got != "+@read" {
		t.Errorf("* modify patched a non-matching element: alice.perms = %v", got)
	}
	if got := users[1].(map[string]any)["perms"]; got != "+@all" {
		t.Errorf("* modify did not patch the matching element: bob.perms = %v", got)
	}
}

func TestState_RemoveDropsMatchingElements(t *testing.T) {
	fs := storeWith(map[string]any{"users": []any{
		map[string]any{"name": "alice"}, map[string]any{"name": "bob"},
	}})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StateRemove, withEvals(runScope(plainSchema())), map[string]any{
		"field": "users",
		"match": `elem.name == "bob"`,
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	users := fs.state["users"].([]any)
	if len(users) != 1 || users[0].(map[string]any)["name"] != "alice" {
		t.Errorf("* remove left %#v, want alice only", users)
	}
	// The KEY must be absent, not merely nil: a nil `effective` is what a verb that
	// failed to resolve would also report.
	if _, reported := ev.GetOutput().AsMap()[coremodstate.OutputEffective]; reported {
		t.Errorf("* remove reported an `effective` value; it writes none: %v", ev.GetOutput().AsMap())
	}
}

// TestState_UnsetDropsTheFieldItself — `unset` removes the key, `remove` removes
// elements from inside it. Two addresses because they are two operations.
func TestState_UnsetDropsTheFieldItself(t *testing.T) {
	fs := storeWith(map[string]any{"users": []any{map[string]any{"name": "alice"}}, "port": float64(6379)})
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StateUnset, runScope(plainSchema()), map[string]any{"field": "users"})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	if _, still := fs.state["users"]; still {
		t.Errorf("* unset left the field behind: %#v", fs.state)
	}
	if fs.state["port"] != float64(6379) {
		t.Errorf("unset touched a neighbouring field: %#v", fs.state)
	}
	if !ev.GetChanged() {
		t.Error("changed = false after dropping a populated field")
	}
}

// TestState_MatchingVerbsRefuseToRunWithoutEvaluators: without an evaluator every
// predicate matches nothing, and a `modify`/`remove` that matched nothing looks
// like a successful no-op. It has to fail instead.
func TestState_MatchingVerbsRefuseToRunWithoutEvaluators(t *testing.T) {
	for _, st := range []string{stateop.StateAdd, stateop.StateModify, stateop.StateRemove} {
		t.Run(st, func(t *testing.T) {
			fs := storeWith(map[string]any{"users": []any{map[string]any{"name": "alice"}}})
			m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(fs)
			params := map[string]any{"field": "users", "match": `elem.name == "alice"`}
			switch st {
			case stateop.StateAdd:
				params["value"] = map[string]any{"name": "alice"}
			case stateop.StateModify:
				params["patch"] = map[string]any{"perms": "+@all"}
			}
			ev := applyState(t, m, st, runScope(plainSchema()), params)
			if !ev.GetFailed() {
				t.Fatalf("* %s ran without merge-time evaluators", st)
			}
		})
	}
}

// TestState_UnknownAddressIsRefused — `core.state` alone has no verb, and a
// misspelt suffix must not fall back to one.
func TestState_UnknownAddressIsRefused(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	for _, st := range []string{"", "sett", "SET", "delete"} {
		t.Run(fmt.Sprintf("%q", st), func(t *testing.T) {
			ev := applyState(t, m, st, runScope(plainSchema()), map[string]any{"field": "port", "value": float64(1)})
			if !ev.GetFailed() {
				t.Fatalf("* state %q was accepted", st)
			}
		})
	}
}

// TestState_ForeignParamIsRefused: a param the verb does not read looks like it
// narrowed the write and did not — `core.state.set` with a `match:` would be a
// wholesale overwrite wearing a filter.
func TestState_ForeignParamIsRefused(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	for _, tc := range []struct {
		state  string
		params map[string]any
	}{
		{stateop.StateSet, map[string]any{"field": "port", "value": float64(1), "match": "x"}},
		{stateop.StateUnset, map[string]any{"field": "port", "value": float64(1)}},
		{stateop.StateAppend, map[string]any{"field": "events", "value": "x", "on_conflict": "skip"}},
	} {
		t.Run(tc.state, func(t *testing.T) {
			ev := applyState(t, m, tc.state, runScope(plainSchema()), tc.params)
			if !ev.GetFailed() {
				t.Fatalf("* %s accepted a param it does not read: %v", tc.state, tc.params)
			}
		})
	}
}

// TestState_ModifyRequiresPatch — a modify with nothing to patch is a no-op the
// author did not intend, and it would report success.
func TestState_ModifyRequiresPatch(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := applyState(t, m, stateop.StateModify, withEvals(runScope(plainSchema())),
		map[string]any{"field": "users", "match": `elem.name == "bob"`})
	if !ev.GetFailed() {
		t.Fatal("* modify without patch: was accepted")
	}
}

// TestState_EnumParamsAreChecked: `on_conflict: replce` must not fall through to
// the engine's default (skip) and silently keep the old element.
func TestState_EnumParamsAreChecked(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	for name, params := range map[string]map[string]any{
		"on_conflict": {"field": "users", "value": map[string]any{"name": "a"}, "on_conflict": "replce"},
		"expect":      {"field": "users", "match": "x", "expect": "exactly_one"},
	} {
		t.Run(name, func(t *testing.T) {
			st := stateop.StateAdd
			if name == "expect" {
				st = stateop.StateRemove
			}
			ev := applyState(t, m, st, withEvals(runScope(plainSchema())), params)
			if !ev.GetFailed() {
				t.Fatalf("* a misspelt %s was accepted", name)
			}
			if !strings.Contains(ev.GetMessage(), name) {
				t.Errorf("message does not name the param: %s", ev.GetMessage())
			}
		})
	}
}

// TestState_AddResolvesTheELEMENTsSecret: `add` writes one element, so the
// declared secret sits one level up from where `set` finds it — and the
// diagnostic path must not claim an index the author never wrote.
func TestState_AddResolvesTheElementsSecret(t *testing.T) {
	fv := newFakeVault(nil)
	fs := storeWith(map[string]any{"redis_users": []any{}})
	m := coremodstate.New(fv, nil, "secret").WithStore(fs)
	ev := applyState(t, m, stateop.StateAdd, withEvals(runScope(collectionSchema())), map[string]any{
		"field": "redis_users",
		"match": `elem.name == value.name`,
		"value": map[string]any{"name": "carol", "password": marker(t, map[string]any{"length": 24})},
	})
	if ev.GetFailed() {
		t.Fatalf("failed: %s", ev.GetMessage())
	}
	const path = "secret/wb-service-redis/redis-prod/redis_users/carol"
	if got, _ := fv.store[path]["password"].(string); len(got) != 24 {
		t.Fatalf("vault %s#password: length %d, want 24", path, len(got))
	}
	elem, ok := ev.GetOutput().AsMap()[coremodstate.OutputEffective].(map[string]any)
	if !ok {
		t.Fatalf("output.effective: want the added element, got %#v", ev.GetOutput().AsMap())
	}
	if got, want := elem["password"], "vault:"+path+"#password"; got != want {
		t.Errorf("effective.password = %v, want %v", got, want)
	}
	// The element landed in state with its secret stripped, not with the value.
	stored := fs.state["redis_users"].([]any)[0].(map[string]any)
	if _, leaked := stored["password"]; leaked {
		t.Errorf("* the secret property reached state: %#v", stored)
	}
}

// TestState_AddOntoAScalarSecretIsRefused — a field that IS one secret has no
// elements to add to; accepting it would derive a path from an absent key.
func TestState_AddOntoAScalarSecretIsRefused(t *testing.T) {
	m := coremodstate.New(newFakeVault(nil), nil, "secret").WithStore(newFakeStore())
	ev := applyState(t, m, stateop.StateAdd, withEvals(runScope(scalarSchema())), map[string]any{
		"field": "admin_password",
		"value": map[string]any{"x": "y"},
	})
	if !ev.GetFailed() {
		t.Fatal("* add onto a scalar secret field was accepted")
	}
}
