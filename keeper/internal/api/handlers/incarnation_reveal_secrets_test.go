package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// makeIncRowSvc is a pgx.Row stub for SelectByName with a custom service (idx 1) and
// state; version "v1". Needed by the prefix-allowlist reveal tests (the service is a
// path segment).
func makeIncRowSvc(name, service string, state map[string]any) pgx.Row {
	stateBytes, _ := json.Marshal(state)
	now := time.Now()
	return staticRow{values: []any{
		name, service, "v1", int(1),
		[]byte("{}"), stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		any(nil), []byte(nil),
		"create", // created_scenario
		any(nil), // applying_apply_id
	}}
}

// fakeVaultReader is a [VaultKVReader] mock for reveal tests: records the requested
// logical paths (to prove the traversal guard cuts BEFORE ReadKV) and returns data/err.
type fakeVaultReader struct {
	data       map[string]any
	err        error
	calledWith []string
}

func (f *fakeVaultReader) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.calledWith = append(f.calledWith, path)
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

// redisUsersState is a typical state with a redis_users array (enumerate source).
func redisUsersState(names ...string) map[string]any {
	users := make([]any, 0, len(names))
	for _, n := range names {
		users = append(users, map[string]any{"name": n, "perms": "~* +@all", "state": "on"})
	}
	return map[string]any{"redis_users": users}
}

// userPasswordSecret is a typical revealable_secrets declaration (redis convention).
// vault_ref is bound to {service}/{incarnation} — resolves to the same
// secret/redis/<inc>/users/<name>#password (service=redis).
func userPasswordSecret() config.RevealableSecret {
	return config.RevealableSecret{
		ID:        "user_password",
		Label:     "Redis user password",
		Enumerate: "state.redis_users",
		VaultRef:  "secret/{service}/{incarnation}/users/{key}#password",
	}
}

// revealHandler builds an IncarnationHandler for reveal tests (db+loader+services+
// scoper+vault+auditW). logger=nil → discard (NewIncarnationHandler).
func revealHandler(state map[string]any, revealable []config.RevealableSecret, vr VaultKVReader, scoper PurviewResolver, aw audit.Writer) *IncarnationHandler {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncRowWithState(name, state) }}
	loader := &fakeLoader{revealableSecrets: revealable}
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, aw, scoper, nil)
	h.SetVaultReader(vr)
	return h
}

func revealClaims() *jwt.Claims { return &jwt.Claims{Subject: "archon-alice"} }

// TestRevealSecret_Happy — success: mock Vault → password; check the value and that
// ReadKV is called with the correct logical path (substitution of {incarnation}/{key}).
func TestRevealSecret_Happy(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t-plaintext"}}
	h := revealHandler(redisUsersState("alice", "bob"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	if err != nil {
		t.Fatalf("RevealSecretTyped: %v", err)
	}
	if res.Value != "s3cr3t-plaintext" {
		t.Errorf("Value = %q, want s3cr3t-plaintext", res.Value)
	}
	if len(vr.calledWith) != 1 || vr.calledWith[0] != "secret/redis/redis-prod/users/alice" {
		t.Errorf("ReadKV called with %#v, want [secret/redis/redis-prod/users/alice]", vr.calledWith)
	}
}

// TestRevealSecret_KeyNotInState_404 — a key outside the enumerate array of the current
// state → 404 (anti-arbitrary), Vault is NOT touched.
func TestRevealSecret_KeyNotInState_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice", "bob"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "carol")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called for a key outside state: %#v", vr.calledWith)
	}
}

// TestRevealSecret_UnknownSecretID_404 — a secret_id not in the manifest → 404.
func TestRevealSecret_UnknownSecretID_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "nonexistent", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called for an unknown secret_id: %#v", vr.calledWith)
	}
}

// TestRevealSecret_OutOfScope_404 — out of RBAC scope → 404 (parity Get, don't leak existence).
func TestRevealSecret_OutOfScope_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called outside scope: %#v", vr.calledWith)
	}
}

// TestRevealSecret_VaultNotFound_404 — ReadKV → ErrVaultKVNotFound (nopass) → 404.
func TestRevealSecret_VaultNotFound_404(t *testing.T) {
	vr := &fakeVaultReader{err: vault.ErrVaultKVNotFound}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_NoField_404 — the secret exists, but the #password field is missing → 404.
func TestRevealSecret_NoField_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"other": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_InvalidKey_422 — a malformed key → 422, Vault is NOT touched.
func TestRevealSecret_InvalidKey_422(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	for _, bad := range []string{"../etc", "Alice", "a/b", ""} {
		_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", bad)
		assertRevealStatus(t, err, 422)
	}
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called with an invalid key: %#v", vr.calledWith)
	}
}

// TestRevealSecret_TraversalGuard — vault.ParseRef cuts `..` EVEN if the manifest
// (author error / malicious) contains traversal in the path template: 404, Vault is NOT called.
func TestRevealSecret_TraversalGuard(t *testing.T) {
	malicious := config.RevealableSecret{
		ID:        "user_password",
		Label:     "x",
		Enumerate: "state.redis_users",
		VaultRef:  "secret/{service}/{incarnation}/../../keeper/{key}#password",
	}
	vr := &fakeVaultReader{data: map[string]any{"password": "leaked"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{malicious}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("traversal-guard breached: ReadKV called with %#v (ParseRef must cut `..` before reading)", vr.calledWith)
	}
}

// TestRevealSecret_LeakGuard — ★ CRITICAL (ADR-064 b): plaintext goes ONLY into the
// response body, NEVER into audit. The payload carries {name, secret_id, key, path} and
// does NOT contain the value; path == logical (not a secret).
func TestRevealSecret_LeakGuard(t *testing.T) {
	const plaintext = "s3cr3t-plaintext-value"
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": plaintext}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, aw)

	res, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	if err != nil {
		t.Fatalf("RevealSecretTyped: %v", err)
	}
	if res.Value != plaintext {
		t.Fatalf("value must be in the response body: %q", res.Value)
	}
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aw.events))
	}
	ev := aw.events[0]
	if ev.EventType != audit.EventIncarnationSecretRevealed {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventIncarnationSecretRevealed)
	}
	if ev.ArchonAID != "archon-alice" {
		t.Errorf("ArchonAID = %q, want archon-alice", ev.ArchonAID)
	}
	// The value must NOT appear ANYWHERE in the payload (serialize the whole thing).
	payloadJSON, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(payloadJSON), plaintext) {
		t.Fatalf("★ LEAK: secret plaintext ended up in audit payload: %s", payloadJSON)
	}
	if ev.Payload["result"] != "ok" {
		t.Errorf("payload result = %#v, want ok", ev.Payload["result"])
	}
	if ev.Payload["name"] != "redis-prod" || ev.Payload["secret_id"] != "user_password" || ev.Payload["key"] != "alice" {
		t.Errorf("payload fields missing/incorrect: %#v", ev.Payload)
	}
	if ev.Payload["path"] != "secret/redis/redis-prod/users/alice" {
		t.Errorf("payload path = %#v, want logical secret/redis/redis-prod/users/alice", ev.Payload["path"])
	}
}

// TestRevealableSecrets_Discovery — discovery returns items with keys from state.
func TestRevealableSecrets_Discovery(t *testing.T) {
	h := revealHandler(redisUsersState("alice", "bob"),
		[]config.RevealableSecret{userPasswordSecret()}, &fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(res.Items))
	}
	it := res.Items[0]
	if it.SecretID != "user_password" || it.Label != "Redis user password" || it.StatePath != "redis_users" {
		t.Errorf("item fields incorrect: %#v", it)
	}
	if strings.Join(it.Keys, ",") != "alice,bob" {
		t.Errorf("keys = %#v, want [alice bob]", it.Keys)
	}
}

// TestRevealableSecrets_OutOfScope_404 — discovery out of scope → 404.
func TestRevealableSecrets_OutOfScope_404(t *testing.T) {
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, &fakeVaultReader{}, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	assertRevealStatus(t, err, 404)
}

// TestRevealableSecrets_NoDeclarations_Empty — a service without revealable_secrets →
// empty list (valid, not an error).
func TestRevealableSecrets_NoDeclarations_Empty(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), nil, &fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("items = %d, want 0 (no declarations)", len(res.Items))
	}
}

// TestRevealSecret_OutOfServiceScope_KeeperSecret_404 — ★ C1 (the main guard):
// a malicious manifest whose path is outside secret/<service>/<incarnation>/ (here —
// straight into secret/keeper/), → reveal 404, ReadKV NOT called (value NOT read), audit
// denied reason=out_of_service_scope.
func TestRevealSecret_OutOfServiceScope_KeeperSecret_404(t *testing.T) {
	evil := config.RevealableSecret{
		ID:        "user_password",
		Label:     "x",
		Enumerate: "state.redis_users",
		VaultRef:  "secret/keeper/{incarnation}/{key}#private_key", // outside secret/redis/redis-prod/
	}
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"private_key": "KEEPER-SIGNING-KEY"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{evil}, vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ SERVICE-SCOPE BREACHED: ReadKV called for a path outside namespace: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["result"] != "denied" || aw.events[0].Payload["reason"] != "out_of_service_scope" {
		t.Errorf("expected audit denied/out_of_service_scope: %#v", aw.events)
	}
	pj, _ := json.Marshal(aw.events)
	if strings.Contains(string(pj), "KEEPER-SIGNING-KEY") {
		t.Fatalf("★ LEAK: value in denied-audit: %s", pj)
	}
}

// TestRevealSecret_OutOfServiceScope_SiblingService_404 — a path under a DIFFERENT
// service (secret/foo/…), even with {service}/{incarnation} somewhere in the path, →
// out_of_service_scope.
func TestRevealSecret_OutOfServiceScope_SiblingService_404(t *testing.T) {
	evil := config.RevealableSecret{
		ID: "user_password", Label: "x", Enumerate: "state.redis_users",
		VaultRef: "secret/foo/{service}/{incarnation}/{key}#password", // → secret/foo/redis/redis-prod/alice
	}
	vr := &fakeVaultReader{data: map[string]any{"password": "leak"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{evil}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ SERVICE-SCOPE BREACHED: ReadKV called for secret/foo/*: %#v", vr.calledWith)
	}
}

// TestRevealSecret_PrefixConfusion_404 — trailing `/` in the allowlist: the path of the
// neighbor incarnation redis-prod-other does NOT match scope redis-prod (otherwise
// prefix confusion).
func TestRevealSecret_PrefixConfusion_404(t *testing.T) {
	evil := config.RevealableSecret{
		ID: "user_password", Label: "x", Enumerate: "state.redis_users",
		VaultRef: "secret/{service}/{incarnation}-other/{key}#password", // → secret/redis/redis-prod-other/alice
	}
	vr := &fakeVaultReader{data: map[string]any{"password": "leak"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{evil}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ PREFIX-CONFUSION: redis-prod-other passed scope redis-prod: %#v", vr.calledWith)
	}
}

// TestRevealSecret_FloorBackstop_ServiceNamedKeeper — floor as a backstop: a service
// with the reserved name `keeper` passes the prefix allowlist (secret/keeper/<inc>/),
// but the floor DeniedByVaultFloor cuts it → 404, ReadKV NOT called, reason=floor_denied.
func TestRevealSecret_FloorBackstop_ServiceNamedKeeper(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return makeIncRowSvc(name, "keeper", redisUsersState("alice"))
	}}
	loader := &fakeLoader{revealableSecrets: []config.RevealableSecret{{
		ID: "user_password", Label: "x", Enumerate: "state.redis_users",
		VaultRef: "secret/{service}/{incarnation}/{key}#password", // → secret/keeper/kept/alice
	}}}
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "leak"}}
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, aw, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(vr)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "kept", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ FLOOR-BACKSTOP BREACHED: ReadKV called for secret/keeper/*: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "floor_denied" {
		t.Errorf("expected audit denied/floor_denied: %#v", aw.events)
	}
}

// TestRevealSecret_DeniedAudit_KeyNotInState — B: a denied reveal (key not in state)
// writes audit with a reason and WITHOUT the value.
func TestRevealSecret_DeniedAudit_KeyNotInState(t *testing.T) {
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "bob")
	assertRevealStatus(t, err, 404)
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (denied)", len(aw.events))
	}
	ev := aw.events[0]
	if ev.Payload["result"] != "denied" || ev.Payload["reason"] != "key_not_in_state" {
		t.Errorf("payload = %#v, want result=denied reason=key_not_in_state", ev.Payload)
	}
	if ev.Payload["name"] != "redis-prod" || ev.Payload["secret_id"] != "user_password" || ev.Payload["key"] != "bob" {
		t.Errorf("payload identifiers incorrect: %#v", ev.Payload)
	}
	pj, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(pj), "s3cr3t") {
		t.Fatalf("★ LEAK: value in denied-audit: %s", pj)
	}
}

// TestRevealSecret_DeniedAudit_OutOfScope — B: out of scope writes audit denied/out_of_scope.
func TestRevealSecret_DeniedAudit_OutOfScope(t *testing.T) {
	aw := &fakeAuditWriter{}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, &fakeVaultReader{}, fakeIncScoper{empty: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "out_of_scope" {
		t.Errorf("expected audit denied/out_of_scope: %#v", aw.events)
	}
}

// TestRevealSecret_VersionCraft_PinsServiceVersion — ★ anti version-craft: the manifest
// is loaded STRICTLY by inc.ServiceVersion (not by the resolver default).
func TestRevealSecret_VersionCraft_PinsServiceVersion(t *testing.T) {
	const wantVersion = "v2.0.0" // different from fakeResolver default ("v1")
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return makeIncRowWithStateVersion(name, wantVersion, redisUsersState("alice"))
	}}
	loader := &fakeLoader{revealableSecrets: []config.RevealableSecret{userPasswordSecret()}}
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, &fakeAuditWriter{}, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(&fakeVaultReader{data: map[string]any{"password": "x"}})

	if _, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice"); err != nil {
		t.Fatalf("RevealSecretTyped: %v", err)
	}
	if len(loader.loadedRefs) == 0 {
		t.Fatal("snapshot was not loaded - version-pin unprovable")
	}
	for i, ref := range loader.loadedRefs {
		if ref != wantVersion {
			t.Errorf("Load[%d] ref = %q, want %q (the manifest must pin to ServiceVersion)", i, ref, wantVersion)
		}
	}
}

// TestRevealSecret_EnumerateNotArray_404 — E: enumerate points to a non-array
// (map/scalar) → 404 (key_not_in_state), without panic and without 500.
func TestRevealSecret_EnumerateNotArray_404(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"not": "an-array"}}
	h := revealHandler(state, []config.RevealableSecret{userPasswordSecret()},
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_BadName_422 / bad secret_id — E: 422 on malformed identifiers.
func TestRevealSecret_BadIdentifiers_422(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), []config.RevealableSecret{userPasswordSecret()},
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})
	// malformed name
	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "Redis_Prod!", "user_password", "alice")
	assertRevealStatus(t, err, 422)
	// malformed secret_id
	_, err = h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "../etc", "alice")
	assertRevealStatus(t, err, 422)
}

// TestRevealableSecrets_DiscoveryFiltersNonConformingKeys — D: a key whose name is
// outside reRevealIdent is NOT advertised by discovery (otherwise reveal would reject it
// with 422).
func TestRevealableSecrets_DiscoveryFiltersNonConformingKeys(t *testing.T) {
	// "Alice" (uppercase) and "" — don't pass reRevealIdent; "bob" — passes.
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "Alice"},
		map[string]any{"name": "bob"},
		map[string]any{"name": ""},
		map[string]any{"perms": "no-name-field"},
	}}
	h := revealHandler(state, []config.RevealableSecret{userPasswordSecret()},
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if strings.Join(res.Items[0].Keys, ",") != "bob" {
		t.Errorf("keys = %#v, want only [bob] (Alice/empty/no-name filtered out)", res.Items[0].Keys)
	}
}

// TestRevealableSecrets_MultipleDeclarations — E: discovery returns ALL declarations.
func TestRevealableSecrets_MultipleDeclarations(t *testing.T) {
	state := map[string]any{
		"redis_users":  []any{map[string]any{"name": "alice"}},
		"admin_tokens": []any{map[string]any{"name": "root"}},
	}
	decls := []config.RevealableSecret{
		userPasswordSecret(),
		{ID: "admin_token", Label: "Admin", Enumerate: "state.admin_tokens", VaultRef: "secret/{service}/{incarnation}/admin/{key}#token"},
	}
	h := revealHandler(state, decls, &fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2 (both declarations)", len(res.Items))
	}
}

// TestEnumerateStateKeys_Coverage — E: unit coverage of enumerateStateKeys —
// nil/empty state, missing array, element without name/name not a string, dedup.
func TestEnumerateStateKeys_Coverage(t *testing.T) {
	// nil state / no key → nil (no panic).
	if got := enumerateStateKeys(nil, "state.redis_users"); got != nil {
		t.Errorf("nil state → %#v, want nil", got)
	}
	if got := enumerateStateKeys(map[string]any{}, "state.redis_users"); got != nil {
		t.Errorf("empty state → %#v, want nil", got)
	}
	// scalar instead of an array → nil.
	if got := enumerateStateKeys(map[string]any{"redis_users": "scalar"}, "state.redis_users"); got != nil {
		t.Errorf("scalar → %#v, want nil", got)
	}
	// element without name / name not a string / duplicates → skip + dedup.
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "alice"},
		map[string]any{"name": "alice"}, // duplicate
		map[string]any{"name": 123},     // not a string
		map[string]any{"perms": "x"},    // no name
		"not-a-map",
	}}
	got := enumerateStateKeys(state, "state.redis_users")
	if strings.Join(got, ",") != "alice" {
		t.Errorf("got %#v, want [alice] (dedup + skip invalid)", got)
	}
}

// assertRevealStatus is a shared assert of a domain *problemError with the expected HTTP status.
func assertRevealStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with status %d, got nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not a problemError: %v", err)
	}
	if d.Status != want {
		t.Errorf("status = %d, want %d (detail=%q)", d.Status, want, d.Detail)
	}
}
