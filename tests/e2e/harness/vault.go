//go:build e2e

package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// Vault test-secrets bootstrap via the direct HTTP API.
//
// Why direct HTTP rather than keeper/internal/vault.Client: the harness is
// a separate go module and is NOT allowed to import `keeper/internal/*`
// (Go internal rules). Vault dev-mode accepts HTTP calls with token auth,
// which is enough for the test environment.
//
// The canon of paths and formats is fixed in [docs/testing/e2e.md -> L3a
// canonical config](../../docs/testing/e2e.md) and ADR-039 Amendment
// 2026-05-26:
//   - JWT signing key: HS256 (HMAC-256), 32 bytes in base64, field
//     `signing_key` at path `secret/data/keeper/jwt-signing-key` (KV v2).
//     NOT an Ed25519 PEM.
//   - PKI mount `pki/`, role `soul-seed` (allow_localhost+allow_any_name
//     for the test environment), root CA TTL 87600h.
//   - Sigil signing key is NOT pre-seeded: KeyService introduces the key
//     at runtime.

const (
	// vaultTokenHeader — the Vault token HTTP header (same for CLI and API).
	vaultTokenHeader = "X-Vault-Token"

	// vaultJWTKeyPath — KV v2 data path for the JWT signing key (see `dev/keeper.dev.yml::auth.jwt.signing_key_ref`).
	// KVv2 requires a `data/` prefix between the mount and the logical path.
	vaultJWTKeyPath = "secret/data/keeper/jwt-signing-key"

	// vaultPKIMountPath — endpoint to enable the PKI mount at `pki/`.
	vaultPKIMountPath = "sys/mounts/pki"

	// vaultPKIRootGenPath — generate-internal root CA.
	vaultPKIRootGenPath = "pki/root/generate/internal"

	// vaultPKIRoleSoulSeed — role for issuing SoulSeed certs (mirrors provision.sh step 5).
	vaultPKIRoleSoulSeed = "pki/roles/soul-seed"

	// vaultPKIIssueSoulSeed — endpoint to issue a leaf cert via the role.
	vaultPKIIssueSoulSeed = "pki/issue/soul-seed"

	// hs256MinBytes — minimum length of an HS256 key (see keeper/internal/jwt/issuer.go::minSigningKeyBytes).
	hs256MinBytes = 32

	// vaultHTTPTimeout — deadline for one Vault HTTP call.
	vaultHTTPTimeout = 10 * time.Second
)

// vaultClient — minimal HTTP client over the Vault dev API.
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

// write does a POST to `<addr>/v1/<path>` with a JSON body; returns the
// decoded `data` field of the response (most write endpoints put the
// payload in `data`). status 204 (No Content, normal for config-mutate
// writes) -> (nil, nil).
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
		// Endpoints with no body (after a rare 200 with no content) — allowed.
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

// InitVaultTestSecrets sets up in Vault the minimal set of test secrets the
// Keeper needs to start in the L3a scenario:
//   - PKI mount + root CA + role `soul-seed`;
//   - JWT signing key (HS256 32B base64) at `secret/keeper/jwt-signing-key`.
//
// `sigil-signing-key` is NOT pre-seeded — KeyService introduces the key
// into the Sigil registry itself via its own API at startup (see ADR-039
// Amendment section 3).
//
// Idempotency is not guaranteed (the harness spins up a fresh Vault
// container per test; calling this again against the same container will
// break on "already mounted" PKI).
func InitVaultTestSecrets(t *testing.T, stack *Stack) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("InitVaultTestSecrets: stack.VaultAddr / vaultToken are empty (NewStack not called?)")
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

	// Root CA (CN=soul-stack-test-ca — isolated namespace from the dev stack `soul-stack`).
	if _, err := vc.write(ctx, vaultPKIRootGenPath, map[string]any{
		"common_name": "soul-stack-test-ca",
		"ttl":         "87600h",
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: generate root CA: %v", err)
	}

	// Role soul-seed: allow_any_name + allow_localhost + IP-SANs for test cases.
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

	// JWT signing key: 32 random bytes in base64.
	jwtKey := generateHS256Key(t)
	if _, err := vc.write(ctx, vaultJWTKeyPath, map[string]any{
		"data": map[string]any{
			"signing_key": jwtKey,
		},
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: write jwt signing-key: %v", err)
	}
}

// SeedVaultKV writes an arbitrary KV v2 secret at logical path
// `secret/<rel>` with the given fields. Used by services that pull a
// secret keeper-side via CEL `vault('secret/<rel>#<field>')` in the render
// phase (e.g. redis-create: `vault('secret/redis/'+incarnation.name+'#password')`,
// ADR-010/ADR-012 — the password reaches the host as a value, the Soul
// vault client does not pull it).
//
// rel — path WITHOUT the mount prefix and WITHOUT `data/` (the KV v2
// infixes are added here): for CEL path `secret/redis/<inc>` pass
// rel = "redis/<inc>". Keeper-side ReadKV normalizes the logical path to
// the KVv2-relative form and reads `secret/data/<rel>`, so the harness
// writes to the same place.
//
// Idempotency is not required (fresh Vault container per test); calling
// again overwrites the secret (KV v2 versioning).
func SeedVaultKV(t *testing.T, stack *Stack, rel string, fields map[string]any) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("SeedVaultKV: stack.VaultAddr / vaultToken are empty (NewStack not called?)")
	}
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()
	// KV v2 PUT: `secret/data/<rel>`, payload wrapped as {"data": {...}}.
	if _, err := vc.write(ctx, "secret/data/"+rel, map[string]any{"data": fields}); err != nil {
		t.Fatalf("SeedVaultKV(%q): %v", rel, err)
	}
}

// generateHS256Key returns 32 random bytes as a base64-encoded string.
// Mirrors `openssl rand -base64 32` in `dev/provision.sh:90`.
//
// bootstrap.extractSigningKey tries base64-decode; on error it accepts the
// string as raw. base64 is used because that's the format Vault KV stores
// binary keys in (string-only fields).
func generateHS256Key(t *testing.T) string {
	t.Helper()
	b := make([]byte, hs256MinBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generateHS256Key: rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// IssueKeeperServerCert issues a leaf cert via Vault PKI for the
// Keeper-server TLS listeners (bootstrap + event_stream). SAN: 127.0.0.1 +
// localhost, TTL 24h.
//
// Mirrors `dev/provision.sh:155-172`. cert/key/caBundle are returned as PEM
// bytes for direct writing to disk before the keeper process starts.
func IssueKeeperServerCert(t *testing.T, stack *Stack) (cert, key, caBundle []byte) {
	t.Helper()
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()

	data, err := vc.write(ctx, vaultPKIIssueSoulSeed, map[string]any{
		"common_name": "localhost",
		"ip_sans":     "127.0.0.1",
		"alt_names":   "localhost",
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

// IssueSoulCert issues a leaf cert for a soul-stub by SID. Used by
// harness.RegisterSoulPreAuth for pre-auth registration of the stub in the
// DB and the subsequent mTLS handshake with the Keeper.
func IssueSoulCert(t *testing.T, stack *Stack, sid string) (cert, key []byte) {
	t.Helper()
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()

	data, err := vc.write(ctx, vaultPKIIssueSoulSeed, map[string]any{
		"common_name": sid,
		"ttl":         "24h",
	})
	if err != nil {
		t.Fatalf("IssueSoulCert(%s): %v", sid, err)
	}
	cert = stringField(t, data, "certificate")
	key = stringField(t, data, "private_key")
	return
}

// stringField extracts a field as []byte (a PEM block).
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
