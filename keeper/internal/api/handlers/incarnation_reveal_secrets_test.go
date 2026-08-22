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
// state; version "v1". Needed by the reveal tests where the service is a derived path
// segment (floor backstop, unsafe-segment defense).
func makeIncRowSvc(name, service string, state map[string]any) pgx.Row {
	stateBytes, _ := json.Marshal(state)
	now := time.Now()
	return staticRow{values: []any{
		name, service, "v1", int(1),
		stateBytes, "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte("{}"), // traits
		"create",     // created_scenario
		any(nil),     // applying_apply_id
	}}
}

// fakeVaultReader is a [VaultKVReader] mock for reveal tests: records the requested
// logical paths (to prove a guard cuts BEFORE ReadKV) and returns data/err.
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

// redisUsersState is a typical state with a redis_users collection.
func redisUsersState(names ...string) map[string]any {
	users := make([]any, 0, len(names))
	for _, n := range names {
		users = append(users, map[string]any{"name": n, "perms": "~* +@all", "state": "on"})
	}
	return map[string]any{"redis_users": users}
}

// redisSecretSchema is the state_schema of a service declaring both supported shapes
// ([ADR-0083] §1): a collection secret (redis_users[].password, addressed by the
// sibling `name`) and a scalar one (admin_password). NO Vault path is authored
// anywhere — that is the point of the ticket; reveal derives it from
// (service, incarnation, state field, key).
func redisSecretSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"redis_users": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"password": map[string]any{"type": "secret", "key": "name", "label": "Redis user password"},
					},
				},
			},
			"admin_password": map[string]any{"type": "secret", "label": "Admin password"},
		},
	}
}

// The two secret_ids redisSecretSchema declares ([config.SecretField.ID]).
const (
	userPasswordID  = "redis_users.password"
	adminPasswordID = "admin_password"
)

// revealHandler builds an IncarnationHandler for reveal tests (db+loader+services+
// scoper+vault+auditW). schema is the snapshot's state_schema — the ONLY source of
// secret declarations. logger=nil → discard (NewIncarnationHandler). Mount "" → the
// default KV mount, so derived paths start with `secret/`.
func revealHandler(state, schema map[string]any, vr VaultKVReader, scoper PurviewResolver, aw audit.Writer) *IncarnationHandler {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncRowWithState(name, state) }}
	loader := &fakeLoader{stateSchema: schema}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true}, loader, aw, scoper, nil)
	h.SetVaultReader(vr, "")
	return h
}

func revealClaims() *jwt.Claims { return &jwt.Claims{Subject: "archon-alice"} }

// TestRevealSecret_Happy — success on a collection secret: mock Vault → password;
// check the value and that ReadKV is called with the DERIVED logical path
// (<mount>/<service>/<incarnation>/<state field>/<key>).
func TestRevealSecret_Happy(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t-plaintext"}}
	h := revealHandler(redisUsersState("alice", "bob"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	if err != nil {
		t.Fatalf("RevealSecretTyped: %v", err)
	}
	if res.Value != "s3cr3t-plaintext" {
		t.Errorf("Value = %q, want s3cr3t-plaintext", res.Value)
	}
	if len(vr.calledWith) != 1 || vr.calledWith[0] != "secret/redis/redis-prod/redis_users/alice" {
		t.Errorf("ReadKV called with %#v, want [secret/redis/redis-prod/redis_users/alice]", vr.calledWith)
	}
}

// TestRevealSecret_ScalarHappy — a scalar `type: secret` field: no key, path without a
// key segment, value under the fixed field name [config.ScalarSecretVaultField].
func TestRevealSecret_ScalarHappy(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"value": "admin-plaintext"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", adminPasswordID, "")
	if err != nil {
		t.Fatalf("RevealSecretTyped: %v", err)
	}
	if res.Value != "admin-plaintext" {
		t.Errorf("Value = %q, want admin-plaintext", res.Value)
	}
	if len(vr.calledWith) != 1 || vr.calledWith[0] != "secret/redis/redis-prod/admin_password" {
		t.Errorf("ReadKV called with %#v, want [secret/redis/redis-prod/admin_password]", vr.calledWith)
	}
}

// TestRevealSecret_ScalarWithKey_422 — a key on a scalar secret is an arity error, not
// "no such secret": 422 with reason key_not_expected, Vault NOT touched. It sits after
// the scope gate, so it IS audited (the one exception to "422s are not audited").
func TestRevealSecret_ScalarWithKey_422(t *testing.T) {
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"value": "admin-plaintext"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", adminPasswordID, "alice")
	assertRevealStatus(t, err, 422)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called for a key on a scalar secret: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "key_not_expected" {
		t.Errorf("expected audit denied/key_not_expected: %#v", aw.events)
	}
}

// TestRevealSecret_CollectionWithoutKey_404 — the mirror arity case: a collection
// secret needs a key, and an empty one addresses no element → 404 key_not_in_state,
// Vault NOT touched. Without this the empty key would fall through to the derivation,
// which is a fail-closed error rather than a diagnosable answer.
func TestRevealSecret_CollectionWithoutKey_404(t *testing.T) {
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called without a key: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "key_not_in_state" {
		t.Errorf("expected audit denied/key_not_in_state: %#v", aw.events)
	}
}

// TestRevealSecret_KeyNotInState_404 — a key outside the collection of the current
// state → 404 (anti-forgery), Vault is NOT touched.
func TestRevealSecret_KeyNotInState_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice", "bob"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "carol")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called for a key outside state: %#v", vr.calledWith)
	}
}

// TestRevealSecret_UnknownSecretID_404 — a secret_id the state_schema does not declare
// → 404.
func TestRevealSecret_UnknownSecretID_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	for _, id := range []string{
		"nonexistent",
		"redis_users",       // the collection itself is not a secret
		"redis_users.perms", // a non-secret property of the element
	} {
		_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", id, "alice")
		assertRevealStatus(t, err, 404)
	}
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called for an unknown secret_id: %#v", vr.calledWith)
	}
}

// TestRevealSecret_OutOfScope_404 — out of RBAC scope → 404 (parity Get, don't leak existence).
func TestRevealSecret_OutOfScope_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called outside scope: %#v", vr.calledWith)
	}
}

// TestRevealSecret_VaultNotFound_404 — ReadKV → ErrVaultKVNotFound (nopass) → 404.
func TestRevealSecret_VaultNotFound_404(t *testing.T) {
	vr := &fakeVaultReader{err: vault.ErrVaultKVNotFound}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_NoField_404 — the secret exists, but the property named by the
// declaration is missing from the KV entry → 404. Guards the field selection too: a
// collection secret is read under its property name, NOT under the scalar constant.
func TestRevealSecret_NoField_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"value": "wrong-field", "other": "x"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_InvalidKey_422 — a key that is not a Vault path segment → 422,
// Vault is NOT touched. The rule is [config.ValidVaultPathSegment] — the SAME one
// core.state.present applies on write, so reveal accepts exactly the keys that can
// have been written.
func TestRevealSecret_InvalidKey_422(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	for _, bad := range []string{"../etc", "a/b", "a#b", ".", "al ice"} {
		_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, bad)
		assertRevealStatus(t, err, 422)
	}
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault must not be called with an invalid key: %#v", vr.calledWith)
	}
}

// TestRevealSecret_UnsafeKeyInState_NeverReachesVault — ★ the traversal guard, restated
// for the derived model. There is no author-written path to poison any more; the only
// attacker-influenceable segment left is the KEY, which comes from state DATA (an
// operator-supplied user name). A state element carrying `../../keeper` must neither be
// advertised by discovery nor be revealable, and Vault must not be touched either way.
func TestRevealSecret_UnsafeKeyInState_NeverReachesVault(t *testing.T) {
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "../../keeper"},
		map[string]any{"name": "alice"},
	}}
	vr := &fakeVaultReader{data: map[string]any{"password": "leaked"}}
	h := revealHandler(state, redisSecretSchema(), vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "../../keeper")
	assertRevealStatus(t, err, 422)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ TRAVERSAL: ReadKV called with %#v (an unsafe state key must be cut before the path is built)", vr.calledWith)
	}

	disc, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	for _, it := range disc.Items {
		for _, k := range it.Keys {
			if k == "../../keeper" {
				t.Fatalf("★ discovery advertised an unsafe key: %#v", it.Keys)
			}
		}
	}
}

// TestRevealSecret_PathIsPerIncarnation — ★ the successor of the prefix-confusion
// guard. The path is no longer authored, so the property to hold is that the
// derivation SEPARATES neighbouring incarnations: `redis-prod` and `redis-prod-other`
// must read two distinct paths, and neither may be a prefix of the other's secret.
// A derivation that dropped or concatenated the incarnation segment fails here.
func TestRevealSecret_PathIsPerIncarnation(t *testing.T) {
	read := func(name string) string {
		vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
		h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
			vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})
		if _, err := h.RevealSecretTyped(context.Background(), revealClaims(), name, userPasswordID, "alice"); err != nil {
			t.Fatalf("RevealSecretTyped(%s): %v", name, err)
		}
		if len(vr.calledWith) != 1 {
			t.Fatalf("ReadKV calls for %s = %#v, want exactly one", name, vr.calledWith)
		}
		return vr.calledWith[0]
	}
	mine, neighbour := read("redis-prod"), read("redis-prod-other")
	if mine != "secret/redis/redis-prod/redis_users/alice" {
		t.Errorf("path = %q, want secret/redis/redis-prod/redis_users/alice", mine)
	}
	if neighbour != "secret/redis/redis-prod-other/redis_users/alice" {
		t.Errorf("path = %q, want secret/redis/redis-prod-other/redis_users/alice", neighbour)
	}
	if strings.HasPrefix(neighbour, strings.TrimSuffix(mine, "/redis_users/alice")+"/") {
		t.Fatalf("★ PREFIX-CONFUSION: %q lives under the namespace of %q", neighbour, mine)
	}
}

// TestRevealSecret_UnsafeServiceSegment_404 — defense in depth on the segment the
// handler does NOT get from the request: a stored incarnation whose service is not a
// safe path segment is refused BEFORE the derivation, Vault untouched. reServiceName
// prevents such a row at registration; this proves the reveal path does not depend on
// that invariant holding.
//
// The absence of an audit event is what pins the layer. The derivation refuses this
// path too, and would audit denied/ref_invalid — attributing to the archon a decision
// about a data anomaly in the stored row, which is an operator-facing log warning
// instead. Drop the handler's own check and this assertion goes red.
func TestRevealSecret_UnsafeServiceSegment_404(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return makeIncRowSvc(name, "redis/../keeper", redisUsersState("alice"))
	}}
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "leaked"}}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true},
		&fakeLoader{stateSchema: redisSecretSchema()}, aw, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(vr, "")

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ PATH INJECTION: ReadKV called with %#v", vr.calledWith)
	}
	if len(aw.events) != 0 {
		t.Errorf("a stored-row anomaly must not be audited as a denied reveal: %#v", aw.events)
	}
}

// TestRevealSecret_LeakGuard — ★ CRITICAL (ADR-064 b): plaintext goes ONLY into the
// response body, NEVER into audit. The payload carries {name, secret_id, key, path} and
// does NOT contain the value; path == logical (a location, not a secret).
func TestRevealSecret_LeakGuard(t *testing.T) {
	const plaintext = "s3cr3t-plaintext-value"
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": plaintext}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, aw)

	res, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
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
	if ev.Payload["name"] != "redis-prod" || ev.Payload["secret_id"] != userPasswordID || ev.Payload["key"] != "alice" {
		t.Errorf("payload fields missing/incorrect: %#v", ev.Payload)
	}
	if ev.Payload["path"] != "secret/redis/redis-prod/redis_users/alice" {
		t.Errorf("payload path = %#v, want logical secret/redis/redis-prod/redis_users/alice", ev.Payload["path"])
	}
}

// TestRevealableSecrets_Discovery — discovery returns one item per declaration, in
// declaration-path order (CollectSecretFields sorts), with the keys of the current
// state and the collection flag that tells an empty collection from a scalar.
func TestRevealableSecrets_Discovery(t *testing.T) {
	h := revealHandler(redisUsersState("alice", "bob"), redisSecretSchema(),
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2 (scalar + collection)", len(res.Items))
	}

	scalar := res.Items[0]
	if scalar.SecretID != adminPasswordID || scalar.Label != "Admin password" || scalar.StatePath != "admin_password" {
		t.Errorf("scalar item fields incorrect: %#v", scalar)
	}
	if scalar.Collection {
		t.Errorf("scalar item must not be a collection: %#v", scalar)
	}
	if len(scalar.Keys) != 0 {
		t.Errorf("scalar keys = %#v, want empty", scalar.Keys)
	}

	coll := res.Items[1]
	if coll.SecretID != userPasswordID || coll.Label != "Redis user password" || coll.StatePath != "redis_users" {
		t.Errorf("collection item fields incorrect: %#v", coll)
	}
	if !coll.Collection {
		t.Errorf("collection item must be flagged: %#v", coll)
	}
	if strings.Join(coll.Keys, ",") != "alice,bob" {
		t.Errorf("keys = %#v, want [alice bob]", coll.Keys)
	}
}

// TestRevealableSecrets_OutOfScope_404 — discovery out of scope → 404.
func TestRevealableSecrets_OutOfScope_404(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		&fakeVaultReader{}, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	assertRevealStatus(t, err, 404)
}

// TestRevealableSecrets_NoDeclarations_Empty — a service whose state_schema declares no
// `type: secret` → empty list (valid, not an error).
func TestRevealableSecrets_NoDeclarations_Empty(t *testing.T) {
	plain := map[string]any{"type": "object", "properties": map[string]any{
		"redis_users": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}},
		}},
	}}
	for label, schema := range map[string]map[string]any{"no-secrets": plain, "no-schema": nil} {
		h := revealHandler(redisUsersState("alice"), schema,
			&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

		res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
		if err != nil {
			t.Fatalf("%s: RevealableSecretsTyped: %v", label, err)
		}
		if len(res.Items) != 0 {
			t.Errorf("%s: items = %d, want 0", label, len(res.Items))
		}
	}
}

// TestRevealSecret_FloorBackstop_ServiceNamedKeeper — the floor as a backstop: a
// service with the reserved name `keeper` derives into secret/keeper/<inc>/…, which
// satisfies the positive allowlist by construction, and is cut by
// [config.DeniedByVaultFloor] → 404, ReadKV NOT called, reason=floor_denied. This is
// the one path where the floor is not redundant with the derivation.
func TestRevealSecret_FloorBackstop_ServiceNamedKeeper(t *testing.T) {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return makeIncRowSvc(name, "keeper", redisUsersState("alice"))
	}}
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "leak"}}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true},
		&fakeLoader{stateSchema: redisSecretSchema()}, aw, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(vr, "")

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "kept", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ FLOOR-BACKSTOP BREACHED: ReadKV called for secret/keeper/*: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "floor_denied" {
		t.Errorf("expected audit denied/floor_denied: %#v", aw.events)
	}
}

// TestRevealSecret_DeniedAudit_KeyNotInState — a denied reveal (key not in state)
// writes audit with a reason and WITHOUT the value.
func TestRevealSecret_DeniedAudit_KeyNotInState(t *testing.T) {
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"password": "s3cr3t"}}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "bob")
	assertRevealStatus(t, err, 404)
	if len(aw.events) != 1 {
		t.Fatalf("audit events = %d, want 1 (denied)", len(aw.events))
	}
	ev := aw.events[0]
	if ev.Payload["result"] != "denied" || ev.Payload["reason"] != "key_not_in_state" {
		t.Errorf("payload = %#v, want result=denied reason=key_not_in_state", ev.Payload)
	}
	if ev.Payload["name"] != "redis-prod" || ev.Payload["secret_id"] != userPasswordID || ev.Payload["key"] != "bob" {
		t.Errorf("payload identifiers incorrect: %#v", ev.Payload)
	}
	pj, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(pj), "s3cr3t") {
		t.Fatalf("★ LEAK: value in denied-audit: %s", pj)
	}
}

// TestRevealSecret_DeniedAudit_OutOfScope — out of scope writes audit denied/out_of_scope.
func TestRevealSecret_DeniedAudit_OutOfScope(t *testing.T) {
	aw := &fakeAuditWriter{}
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		&fakeVaultReader{}, fakeIncScoper{empty: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
	assertRevealStatus(t, err, 404)
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "out_of_scope" {
		t.Errorf("expected audit denied/out_of_scope: %#v", aw.events)
	}
}

// TestRevealSecret_VersionCraft_PinsServiceVersion — ★ anti version-craft: the
// state_schema is loaded STRICTLY at inc.ServiceVersion (not at the resolver default).
// A stale pin would otherwise declare secrets the current state no longer has.
func TestRevealSecret_VersionCraft_PinsServiceVersion(t *testing.T) {
	const wantVersion = "v2.0.0" // different from fakeResolver default ("v1")
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row {
		return makeIncRowWithStateVersion(name, wantVersion, redisUsersState("alice"))
	}}
	loader := &fakeLoader{stateSchema: redisSecretSchema()}
	h := NewIncarnationHandler(db, nil, nil, &fakeResolver{ok: true}, loader, &fakeAuditWriter{}, fakeIncScoper{unrestricted: true}, nil)
	h.SetVaultReader(&fakeVaultReader{data: map[string]any{"password": "x"}}, "")

	if _, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice"); err != nil {
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

// TestRevealSecret_CollectionNotArray_404 — the state field of a collection secret
// holds a non-array (map/scalar) → 404 (key_not_in_state), without panic and without 500.
func TestRevealSecret_CollectionNotArray_404(t *testing.T) {
	for _, state := range []map[string]any{
		{"redis_users": map[string]any{"not": "an-array"}},
		{"redis_users": "scalar"},
		{},
	} {
		h := revealHandler(state, redisSecretSchema(),
			&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

		_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", userPasswordID, "alice")
		assertRevealStatus(t, err, 404)
	}
}

// TestRevealSecret_BadIdentifiers_422 — 422 on malformed identifiers: the incarnation
// name, and a secret_id outside the `<field>` / `<field>.<property>` form the
// derivation could ever produce.
func TestRevealSecret_BadIdentifiers_422(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), redisSecretSchema(),
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})
	// malformed name
	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "Redis_Prod!", userPasswordID, "alice")
	assertRevealStatus(t, err, 422)
	// malformed secret_id: traversal, a slash, and one dot too many
	for _, id := range []string{"../etc", "a/b", "a.b.c", ""} {
		_, err = h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", id, "alice")
		assertRevealStatus(t, err, 422)
	}
}

// TestRevealableSecrets_DiscoveryFiltersNonConformingKeys — a state key that is not a
// Vault path segment is NOT advertised (reveal would 422 it). The filter is
// [config.ValidVaultPathSegment], so an uppercase name IS advertised: discovery and
// reveal must agree, and core.state.present writes under that same rule.
func TestRevealableSecrets_DiscoveryFiltersNonConformingKeys(t *testing.T) {
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "../etc"},         // traversal
		map[string]any{"name": "a/b"},            // separator
		map[string]any{"name": ""},               // empty
		map[string]any{"name": 123},              // not a string
		map[string]any{"perms": "no-name-field"}, // no key property
		map[string]any{"name": "Alice"},          // uppercase — legal segment, advertised
		map[string]any{"name": "bob"},            // plain
		map[string]any{"name": "bob"},            // duplicate
	}}
	h := revealHandler(state, redisSecretSchema(),
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	coll := res.Items[1] // [0] is the scalar admin_password
	if strings.Join(coll.Keys, ",") != "Alice,bob" {
		t.Errorf("keys = %#v, want [Alice bob] (unsafe/empty/non-string/missing filtered, duplicate deduped)", coll.Keys)
	}
}

// TestEnumerateStateKeys_Coverage — unit coverage of enumerateStateKeys: a scalar
// field has no keys, nil/empty state and a non-array field yield nil (no panic), and
// invalid elements are skipped while duplicates are deduped.
func TestEnumerateStateKeys_Coverage(t *testing.T) {
	fields, issues := config.CollectSecretFields(redisSecretSchema())
	if len(issues) != 0 || len(fields) != 2 {
		t.Fatalf("fixture schema is not the expected two declarations: fields=%#v issues=%#v", fields, issues)
	}
	scalar, coll := fields[0], fields[1]

	// A scalar field addresses no element → nil regardless of state.
	if got := enumerateStateKeys(redisUsersState("alice"), scalar); got != nil {
		t.Errorf("scalar field → %#v, want nil", got)
	}
	// nil state / missing field / non-array → nil (no panic).
	if got := enumerateStateKeys(nil, coll); got != nil {
		t.Errorf("nil state → %#v, want nil", got)
	}
	if got := enumerateStateKeys(map[string]any{}, coll); got != nil {
		t.Errorf("empty state → %#v, want nil", got)
	}
	if got := enumerateStateKeys(map[string]any{"redis_users": "scalar"}, coll); got != nil {
		t.Errorf("scalar → %#v, want nil", got)
	}
	// element without the key property / key not a string / duplicates → skip + dedup.
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "alice"},
		map[string]any{"name": "alice"}, // duplicate
		map[string]any{"name": 123},     // not a string
		map[string]any{"perms": "x"},    // no name
		"not-a-map",
	}}
	if got := enumerateStateKeys(state, coll); strings.Join(got, ",") != "alice" {
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
