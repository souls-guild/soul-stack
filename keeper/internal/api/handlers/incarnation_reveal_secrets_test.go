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

// makeIncRowSvc — pgx.Row-stub под SelectByName с кастомным service (idx 1) и
// state; version "v1". Нужен reveal-тестам prefix-allowlist (сервис — сегмент пути).
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

// fakeVaultReader — мок [VaultKVReader] для reveal-тестов: фиксирует запрошенные
// logical-пути (доказать, что traversal-guard режет ДО ReadKV) и отдаёт data/err.
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

// redisUsersState — типовой state с массивом redis_users (enumerate-источник).
func redisUsersState(names ...string) map[string]any {
	users := make([]any, 0, len(names))
	for _, n := range names {
		users = append(users, map[string]any{"name": n, "perms": "~* +@all", "state": "on"})
	}
	return map[string]any{"redis_users": users}
}

// userPasswordSecret — типовая декларация revealable_secrets (redis-конвенция).
// vault_ref привязан к {service}/{incarnation} — резолвится в тот же
// secret/redis/<inc>/users/<name>#password (сервис=redis).
func userPasswordSecret() config.RevealableSecret {
	return config.RevealableSecret{
		ID:        "user_password",
		Label:     "Пароль пользователя Redis",
		Enumerate: "state.redis_users",
		VaultRef:  "secret/{service}/{incarnation}/users/{key}#password",
	}
}

// revealHandler собирает IncarnationHandler под reveal-тесты (db+loader+services+
// scoper+vault+auditW). logger=nil → discard (NewIncarnationHandler).
func revealHandler(state map[string]any, revealable []config.RevealableSecret, vr VaultKVReader, scoper PurviewResolver, aw audit.Writer) *IncarnationHandler {
	db := &fakeIncDB{selectByNameRow: func(name string) pgx.Row { return makeIncRowWithState(name, state) }}
	loader := &fakeLoader{revealableSecrets: revealable}
	h := NewIncarnationHandler(db, nil, nil, nil, &fakeResolver{ok: true}, loader, aw, scoper, nil)
	h.SetVaultReader(vr)
	return h
}

func revealClaims() *jwt.Claims { return &jwt.Claims{Subject: "archon-alice"} }

// TestRevealSecret_Happy — успех: mock Vault → пароль; проверяем значение и что
// ReadKV вызван с корректным logical-путём (подстановка {incarnation}/{key}).
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

// TestRevealSecret_KeyNotInState_404 — key вне enumerate-массива текущего state →
// 404 (анти-произвол), Vault НЕ трогаем.
func TestRevealSecret_KeyNotInState_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice", "bob"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "carol")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault не должен вызываться для key вне state: %#v", vr.calledWith)
	}
}

// TestRevealSecret_UnknownSecretID_404 — secret_id, которого нет в манифесте → 404.
func TestRevealSecret_UnknownSecretID_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "nonexistent", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault не должен вызываться для неизвестного secret_id: %#v", vr.calledWith)
	}
}

// TestRevealSecret_OutOfScope_404 — вне RBAC-scope → 404 (parity Get, не палим существование).
func TestRevealSecret_OutOfScope_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault не должен вызываться вне scope: %#v", vr.calledWith)
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

// TestRevealSecret_NoField_404 — секрет есть, но поля #password нет → 404.
func TestRevealSecret_NoField_404(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"other": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_InvalidKey_422 — key невалидной формы → 422, Vault НЕ трогаем.
func TestRevealSecret_InvalidKey_422(t *testing.T) {
	vr := &fakeVaultReader{data: map[string]any{"password": "x"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, vr, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	for _, bad := range []string{"../etc", "Alice", "a/b", ""} {
		_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", bad)
		assertRevealStatus(t, err, 422)
	}
	if len(vr.calledWith) != 0 {
		t.Errorf("Vault не должен вызываться при невалидном key: %#v", vr.calledWith)
	}
}

// TestRevealSecret_TraversalGuard — vault.ParseRef режет `..` ДАЖЕ если манифест
// (author-error / вредоносный) содержит traversal в шаблоне пути: 404, Vault НЕ вызван.
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
		t.Errorf("traversal-guard пробит: ReadKV вызван с %#v (ParseRef обязан отсечь `..` до чтения)", vr.calledWith)
	}
}

// TestRevealSecret_LeakGuard — ★ КРИТИЧНО (ADR-064 b): plaintext уходит ТОЛЬКО в
// тело ответа, НИКОГДА в audit. Payload несёт {name, secret_id, key, path} и НЕ
// содержит значения; path == logical (не секрет).
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
		t.Fatalf("значение обязано быть в теле ответа: %q", res.Value)
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
	// Значение НЕ должно присутствовать НИГДЕ в payload (сериализуем целиком).
	payloadJSON, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(payloadJSON), plaintext) {
		t.Fatalf("★ УТЕЧКА: plaintext секрета попал в audit-payload: %s", payloadJSON)
	}
	if ev.Payload["result"] != "ok" {
		t.Errorf("payload result = %#v, want ok", ev.Payload["result"])
	}
	if ev.Payload["name"] != "redis-prod" || ev.Payload["secret_id"] != "user_password" || ev.Payload["key"] != "alice" {
		t.Errorf("payload полей не хватает/неверны: %#v", ev.Payload)
	}
	if ev.Payload["path"] != "secret/redis/redis-prod/users/alice" {
		t.Errorf("payload path = %#v, want logical secret/redis/redis-prod/users/alice", ev.Payload["path"])
	}
}

// TestRevealableSecrets_Discovery — discovery отдаёт items с keys из state.
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
	if it.SecretID != "user_password" || it.Label != "Пароль пользователя Redis" || it.StatePath != "redis_users" {
		t.Errorf("item поля неверны: %#v", it)
	}
	if strings.Join(it.Keys, ",") != "alice,bob" {
		t.Errorf("keys = %#v, want [alice bob]", it.Keys)
	}
}

// TestRevealableSecrets_OutOfScope_404 — discovery вне scope → 404.
func TestRevealableSecrets_OutOfScope_404(t *testing.T) {
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, &fakeVaultReader{}, fakeIncScoper{empty: true}, &fakeAuditWriter{})

	_, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	assertRevealStatus(t, err, 404)
}

// TestRevealableSecrets_NoDeclarations_Empty — сервис без revealable_secrets →
// пустой список (валиден, не ошибка).
func TestRevealableSecrets_NoDeclarations_Empty(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), nil, &fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	res, err := h.RevealableSecretsTyped(context.Background(), revealClaims(), "redis-prod")
	if err != nil {
		t.Fatalf("RevealableSecretsTyped: %v", err)
	}
	if len(res.Items) != 0 {
		t.Errorf("items = %d, want 0 (нет деклараций)", len(res.Items))
	}
}

// TestRevealSecret_OutOfServiceScope_KeeperSecret_404 — ★ C1 (главный guard):
// вредоносный манифест, чей путь вне secret/<service>/<incarnation>/ (тут — прямо
// в secret/keeper/), → reveal 404, ReadKV НЕ вызван (значение НЕ прочитано), audit
// denied reason=out_of_service_scope.
func TestRevealSecret_OutOfServiceScope_KeeperSecret_404(t *testing.T) {
	evil := config.RevealableSecret{
		ID:        "user_password",
		Label:     "x",
		Enumerate: "state.redis_users",
		VaultRef:  "secret/keeper/{incarnation}/{key}#private_key", // вне secret/redis/redis-prod/
	}
	aw := &fakeAuditWriter{}
	vr := &fakeVaultReader{data: map[string]any{"private_key": "KEEPER-SIGNING-KEY"}}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{evil}, vr, fakeIncScoper{unrestricted: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(vr.calledWith) != 0 {
		t.Fatalf("★ SERVICE-SCOPE ПРОБИТ: ReadKV вызван для пути вне неймспейса: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["result"] != "denied" || aw.events[0].Payload["reason"] != "out_of_service_scope" {
		t.Errorf("ожидался audit denied/out_of_service_scope: %#v", aw.events)
	}
	pj, _ := json.Marshal(aw.events)
	if strings.Contains(string(pj), "KEEPER-SIGNING-KEY") {
		t.Fatalf("★ УТЕЧКА: значение в denied-audit: %s", pj)
	}
}

// TestRevealSecret_OutOfServiceScope_SiblingService_404 — путь под ДРУГИМ сервисом
// (secret/foo/…), хоть и с {service}/{incarnation} где-то в пути, → out_of_service_scope.
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
		t.Fatalf("★ SERVICE-SCOPE ПРОБИТ: ReadKV вызван для secret/foo/*: %#v", vr.calledWith)
	}
}

// TestRevealSecret_PrefixConfusion_404 — trailing `/` в allowlist: путь соседней
// инкарнации redis-prod-other НЕ матчит scope redis-prod (иначе prefix-confusion).
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
		t.Fatalf("★ PREFIX-CONFUSION: redis-prod-other прошёл scope redis-prod: %#v", vr.calledWith)
	}
}

// TestRevealSecret_FloorBackstop_ServiceNamedKeeper — floor как backstop: сервис с
// зарезервированным именем `keeper` проходит prefix-allowlist (secret/keeper/<inc>/),
// но floor DeniedByVaultFloor его режет → 404, ReadKV НЕ вызван, reason=floor_denied.
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
		t.Fatalf("★ FLOOR-BACKSTOP ПРОБИТ: ReadKV вызван для secret/keeper/*: %#v", vr.calledWith)
	}
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "floor_denied" {
		t.Errorf("ожидался audit denied/floor_denied: %#v", aw.events)
	}
}

// TestRevealSecret_DeniedAudit_KeyNotInState — B: denied-reveal (key не в state)
// пишет audit с reason и БЕЗ значения.
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
		t.Errorf("payload идентификаторов неверен: %#v", ev.Payload)
	}
	pj, _ := json.Marshal(ev.Payload)
	if strings.Contains(string(pj), "s3cr3t") {
		t.Fatalf("★ УТЕЧКА: значение в denied-audit: %s", pj)
	}
}

// TestRevealSecret_DeniedAudit_OutOfScope — B: вне scope пишет audit denied/out_of_scope.
func TestRevealSecret_DeniedAudit_OutOfScope(t *testing.T) {
	aw := &fakeAuditWriter{}
	h := revealHandler(redisUsersState("alice"),
		[]config.RevealableSecret{userPasswordSecret()}, &fakeVaultReader{}, fakeIncScoper{empty: true}, aw)

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
	if len(aw.events) != 1 || aw.events[0].Payload["reason"] != "out_of_scope" {
		t.Errorf("ожидался audit denied/out_of_scope: %#v", aw.events)
	}
}

// TestRevealSecret_VersionCraft_PinsServiceVersion — ★ анти version-craft: манифест
// грузится СТРОГО по inc.ServiceVersion (не по дефолту резолвера).
func TestRevealSecret_VersionCraft_PinsServiceVersion(t *testing.T) {
	const wantVersion = "v2.0.0" // отлично от дефолта fakeResolver ("v1")
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
		t.Fatal("снапшот не загружался — version-pin недоказуем")
	}
	for i, ref := range loader.loadedRefs {
		if ref != wantVersion {
			t.Errorf("Load[%d] ref = %q, want %q (манифест обязан пиниться на ServiceVersion)", i, ref, wantVersion)
		}
	}
}

// TestRevealSecret_EnumerateNotArray_404 — E: enumerate указывает на не-массив
// (map/scalar) → 404 (key_not_in_state), без паники и без 500.
func TestRevealSecret_EnumerateNotArray_404(t *testing.T) {
	state := map[string]any{"redis_users": map[string]any{"not": "an-array"}}
	h := revealHandler(state, []config.RevealableSecret{userPasswordSecret()},
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})

	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "user_password", "alice")
	assertRevealStatus(t, err, 404)
}

// TestRevealSecret_BadName_422 / bad secret_id — E: 422 на битые идентификаторы.
func TestRevealSecret_BadIdentifiers_422(t *testing.T) {
	h := revealHandler(redisUsersState("alice"), []config.RevealableSecret{userPasswordSecret()},
		&fakeVaultReader{}, fakeIncScoper{unrestricted: true}, &fakeAuditWriter{})
	// битый name
	_, err := h.RevealSecretTyped(context.Background(), revealClaims(), "Redis_Prod!", "user_password", "alice")
	assertRevealStatus(t, err, 422)
	// битый secret_id
	_, err = h.RevealSecretTyped(context.Background(), revealClaims(), "redis-prod", "../etc", "alice")
	assertRevealStatus(t, err, 422)
}

// TestRevealableSecrets_DiscoveryFiltersNonConformingKeys — D: ключ с именем вне
// reRevealIdent НЕ рекламируется discovery (иначе reveal отобьёт его 422).
func TestRevealableSecrets_DiscoveryFiltersNonConformingKeys(t *testing.T) {
	// "Alice" (заглавная) и "" — не проходят reRevealIdent; "bob" — проходит.
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
		t.Errorf("keys = %#v, want только [bob] (Alice/пустое/без-name отфильтрованы)", res.Items[0].Keys)
	}
}

// TestRevealableSecrets_MultipleDeclarations — E: discovery отдаёт ВСЕ декларации.
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
		t.Fatalf("items = %d, want 2 (обе декларации)", len(res.Items))
	}
}

// TestEnumerateStateKeys_Coverage — E: unit-покрытие enumerateStateKeys —
// nil/пустой state, отсутствие массива, элемент без name/name не строка, дедуп.
func TestEnumerateStateKeys_Coverage(t *testing.T) {
	// nil state / нет ключа → nil (без паники).
	if got := enumerateStateKeys(nil, "state.redis_users"); got != nil {
		t.Errorf("nil state → %#v, want nil", got)
	}
	if got := enumerateStateKeys(map[string]any{}, "state.redis_users"); got != nil {
		t.Errorf("empty state → %#v, want nil", got)
	}
	// scalar вместо массива → nil.
	if got := enumerateStateKeys(map[string]any{"redis_users": "scalar"}, "state.redis_users"); got != nil {
		t.Errorf("scalar → %#v, want nil", got)
	}
	// элемент без name / name не строка / дубли → пропуск + дедуп.
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "alice"},
		map[string]any{"name": "alice"}, // дубль
		map[string]any{"name": 123},     // не строка
		map[string]any{"perms": "x"},    // нет name
		"not-a-map",
	}}
	got := enumerateStateKeys(state, "state.redis_users")
	if strings.Join(got, ",") != "alice" {
		t.Errorf("got %#v, want [alice] (дедуп + пропуск невалидных)", got)
	}
}

// assertRevealStatus — общий assert доменного *problemError с ожидаемым HTTP-статусом.
func assertRevealStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("ожидалась ошибка со статусом %d, got nil", want)
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не problemError: %v", err)
	}
	if d.Status != want {
		t.Errorf("status = %d, want %d (detail=%q)", d.Status, want, d.Detail)
	}
}
