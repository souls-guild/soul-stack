//go:build e2e_live

package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Vault test-secrets bootstrap через прямой HTTP API.
//
// Зачем direct HTTP, а не keeper/internal/vault.Client: harness — отдельный
// go-модуль и НЕ имеет права импортировать `keeper/internal/*` (Go internal-
// rules). Vault dev-mode принимает HTTP-вызовы по token-auth, чего для test-
// окружения достаточно.
//
// Канон путей и форматов фиксирован [docs/testing/e2e.md → L3a canonical
// config](../../docs/testing/e2e.md) и ADR-039 Amendment:
//   - JWT signing-key: HS256 (HMAC-256), 32 байта в base64, поле `signing_key`
//     по пути `secret/data/keeper/jwt-signing-key` (KV v2). НЕ Ed25519 PEM.
//   - PKI mount `pki/`, role `soul-seed` (allow_localhost+allow_any_name для
//     test-окружения), root CA TTL 87600h.
//   - Sigil signing-key НЕ pre-seed: KeyService introduces ключ runtime.

const (
	// vaultTokenHeader — http-заголовок Vault-token (CLI и API одинаковы).
	vaultTokenHeader = "X-Vault-Token"

	// vaultJWTKeyPath — KV v2 data-path для JWT signing-key (см. `dev/keeper.dev.yml::auth.jwt.signing_key_ref`).
	// KVv2 требует префикс `data/` между mount-ом и логическим путём.
	vaultJWTKeyPath = "secret/data/keeper/jwt-signing-key"

	// sigilSigningKeyRef — config-ссылка `sigil.signing_key_ref` keeper.yml
	// (parity dev/keeper.dev.yml); vaultSigilKeyPath — её KV v2 data-path.
	sigilSigningKeyRef = "vault:secret/keeper/sigil-signing-key"
	vaultSigilKeyPath  = "secret/data/keeper/sigil-signing-key"

	// vaultPKIMountPath — endpoint enable mount-а PKI на пути `pki/`.
	vaultPKIMountPath = "sys/mounts/pki"

	// vaultPKIRootGenPath — generate-internal root CA.
	vaultPKIRootGenPath = "pki/root/generate/internal"

	// vaultPKIRoleSoulSeed — role для выпуска SoulSeed-cert-ов (симметрично provision.sh шаг 5).
	vaultPKIRoleSoulSeed = "pki/roles/soul-seed"

	// vaultPKIIssueSoulSeed — endpoint выпуска leaf-cert-а по role.
	vaultPKIIssueSoulSeed = "pki/issue/soul-seed"

	// hs256MinBytes — минимальная длина HS256 ключа (см. keeper/internal/jwt/issuer.go::minSigningKeyBytes).
	hs256MinBytes = 32

	// vaultHTTPTimeout — deadline на один Vault HTTP-вызов.
	vaultHTTPTimeout = 10 * time.Second
)

// vaultClient — минимальный HTTP-клиент над Vault dev-API.
type vaultClient struct {
	addr  string
	token string
	http  *http.Client
}

func newVaultClient(addr, token string) *vaultClient {
	return &vaultClient{
		addr:  addr,
		token: token,
		http:  &http.Client{Timeout: vaultHTTPTimeout},
	}
}

// write делает POST на `<addr>/v1/<path>` с JSON-body; возвращает декодированное
// `data`-поле ответа (для большинства write-эндпоинтов Vault кладёт payload в `data`).
// status 204 (No Content, normal для config-mutate write-ов) → (nil, nil).
func (vc *vaultClient) write(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("vault write %s: marshal body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vc.addr+"/v1/"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("vault write %s: build request: %w", path, err)
	}
	req.Header.Set(vaultTokenHeader, vc.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := vc.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault write %s: http: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vault write %s: status %d: %s", path, resp.StatusCode, string(b))
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, fmt.Errorf("vault write %s: decode response: %w", path, err)
	}
	if data, ok := out["data"].(map[string]any); ok {
		return data, nil
	}
	return out, nil
}

// InitVaultTestSecrets поднимает в Vault минимальный набор test-secrets,
// необходимый Keeper-у для старта в L3b-сценарии:
//   - PKI mount + root CA + role `soul-seed`;
//   - JWT signing-key (HS256 32B base64) в `secret/keeper/jwt-signing-key`.
//
// `sigil-signing-key` НЕ pre-seed-ится — KeyService при старте сам введёт
// ключ в реестр Sigil через свой API (см. ADR-039 Amendment §3).
//
// L3b-отличие от L3a: PKI-role `soul-seed` будет использоваться также для
// выпуска cert-а реальному soul-агенту через CSR-Bootstrap-flow (L3b-2), а
// не только для harness-side mTLS.
func InitVaultTestSecrets(t *testing.T, stack *Stack) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("InitVaultTestSecrets: stack.VaultAddr / vaultToken пустые (NewStack не вызван?)")
	}
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// PKI mount.
	if _, err := vc.write(ctx, vaultPKIMountPath, map[string]any{
		"type": "pki",
		"config": map[string]any{
			"max_lease_ttl": "87600h",
		},
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: enable pki mount: %v", err)
	}

	// Root CA (CN=soul-stack-test-ca — изолированный namespace от dev-стека `soul-stack`).
	if _, err := vc.write(ctx, vaultPKIRootGenPath, map[string]any{
		"common_name": "soul-stack-test-ca",
		"ttl":         "87600h",
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: generate root CA: %v", err)
	}

	// Role soul-seed: allow_any_name + allow_localhost + IP-SANs для test-кейсов.
	if _, err := vc.write(ctx, vaultPKIRoleSoulSeed, map[string]any{
		"allowed_domains":  "example.com,localhost",
		"allow_subdomains": true,
		"allow_any_name":   true,
		"allow_localhost":  true,
		"allow_ip_sans":    true,
		"max_ttl":          "720h",
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: create role soul-seed: %v", err)
	}

	// JWT signing-key: 32 байта random в base64.
	jwtKey := generateHS256Key(t)
	if _, err := vc.write(ctx, vaultJWTKeyPath, map[string]any{
		"data": map[string]any{
			"signing_key": jwtKey,
		},
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: write jwt signing-key: %v", err)
	}
}

// SeedSigilSigningKey кладёт ed25519-ключ подписи Sigil (ADR-026) в Vault KV
// `secret/keeper/sigil-signing-key`, поле `signing_key` — base64 от raw
// 32-байтного seed (одна из форм keeper/internal/sigil/key.go::parseEd25519Key).
//
// Вызывается NewStack-ом ТОЛЬКО при непустом cfg.SoulModules: sigil-блок в
// keeper.yml без этого секрета валит `keeper run` на setupSigil (cfg-fallback
// buildSigilSigner-а читает ключ при старте). Канон ADR-039 §3 «sigil-key НЕ
// pre-seed» остаётся для стендов без plugin-канала — InitVaultTestSecrets
// этот ключ по-прежнему не сеет.
func SeedSigilSigningKey(t *testing.T, stack *Stack) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("SeedSigilSigningKey: stack.VaultAddr / vaultToken пустые (NewStack не вызван?)")
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("SeedSigilSigningKey: rand.Read: %v", err)
	}
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()
	if _, err := vc.write(ctx, vaultSigilKeyPath, map[string]any{
		"data": map[string]any{
			"signing_key": base64.StdEncoding.EncodeToString(seed),
		},
	}); err != nil {
		t.Fatalf("SeedSigilSigningKey: write sigil signing-key: %v", err)
	}
}

// SeedVaultKV кладёт произвольный KV v2-секрет по logical-пути `secret/<rel>` с
// полями fields. Используется сервисами, которые тянут секрет keeper-side через
// CEL `vault('secret/<rel>#<field>')` в render-фазе (redis-create:
// `vault('secret/redis/'+incarnation.name+'#password')`, ADR-010/ADR-012 — пароль
// доезжает на хост значением, Soul vault-клиент не тянет).
//
// rel — путь БЕЗ mount-префикса и БЕЗ `data/` (KV v2-инфиксы добавляются тут):
// для CEL-пути `secret/redis/<inc>` передавать rel = "redis/<inc>". Keeper-side
// ReadKV нормализует logical-путь к KVv2-relative и читает `secret/data/<rel>`,
// поэтому harness пишет туда же.
func SeedVaultKV(t *testing.T, stack *Stack, rel string, fields map[string]any) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("SeedVaultKV: stack.VaultAddr / vaultToken пустые (NewStack не вызван?)")
	}
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()
	if _, err := vc.write(ctx, "secret/data/"+rel, map[string]any{"data": fields}); err != nil {
		t.Fatalf("SeedVaultKV(%q): %v", rel, err)
	}
}

// generateHS256Key возвращает 32 случайных байта в base64-encoded строке.
// Симметрично `openssl rand -base64 32` в `dev/provision.sh:90`.
func generateHS256Key(t *testing.T) string {
	t.Helper()
	b := make([]byte, hs256MinBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generateHS256Key: rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// IssueKeeperServerCert выпускает leaf-cert через Vault PKI для Keeper-server
// TLS-listener-ов (bootstrap + event_stream). SAN: 127.0.0.1 + localhost +
// host.docker.internal (последний — для коннекта из L3b soul-контейнера к
// keeper-у, который слушает на хосте), TTL 24h.
//
// Bootstrap — server-only TLS (ADR-012): соул верифицирует keeper-серт по
// hostname keeper-эндпоинта. Поэтому SAN ОБЯЗАН покрывать host, на который
// соул реально дозванивается (keeperEndpointHost()). При E2E_KEEPER_HOST-override
// (WSL2: реальный хост-IP) — добавляем его в ip_sans/alt_names, иначе соул
// получит TLS-SAN-mismatch вместо connection-refused.
//
// Симметрично `dev/provision.sh:155-172`. cert/key/caBundle возвращаются как
// PEM-байты для прямой записи на диск перед стартом keeper-процесса.
func IssueKeeperServerCert(t *testing.T, stack *Stack) (cert, key, caBundle []byte) {
	t.Helper()
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()

	ipSANs := []string{"127.0.0.1"}
	altNames := []string{"localhost", defaultKeeperHost}
	if host := keeperEndpointHost(); host != defaultKeeperHost {
		if net.ParseIP(host) != nil {
			ipSANs = append(ipSANs, host)
		} else {
			altNames = append(altNames, host)
		}
	}

	data, err := vc.write(ctx, vaultPKIIssueSoulSeed, map[string]any{
		"common_name": "localhost",
		"ip_sans":     strings.Join(ipSANs, ","),
		"alt_names":   strings.Join(altNames, ","),
		"ttl":         "24h",
	})
	if err != nil {
		t.Fatalf("IssueKeeperServerCert: %v", err)
	}
	cert = stringField(t, data, "certificate")
	key = stringField(t, data, "private_key")
	caBundle = stringField(t, data, "issuing_ca")
	return
}

// stringField извлекает поле как []byte (PEM-блок).
func stringField(t *testing.T, data map[string]any, field string) []byte {
	t.Helper()
	v, ok := data[field]
	if !ok {
		t.Fatalf("vault response missing field %q (got keys=%v)", field, mapKeys(data))
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("vault response field %q has type %T, want string", field, v)
	}
	return []byte(s)
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
