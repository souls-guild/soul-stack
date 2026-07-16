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

// Vault test-secrets bootstrap via direct HTTP API.
//
// Why direct HTTP instead of keeper/internal/vault.Client: the harness is a
// separate go module and has no right to import `keeper/internal/*` (Go
// internal rules). Vault dev-mode accepts HTTP calls with token-auth, which is
// enough for a test environment.
//
// The canon of paths and formats is fixed by [docs/testing/e2e.md -> L3a
// canonical config](../../docs/testing/e2e.md) and ADR-039 Amendment:
//   - JWT signing-key: HS256 (HMAC-256), 32 bytes in base64, field `signing_key`
//     at path `secret/data/keeper/jwt-signing-key` (KV v2). NOT Ed25519 PEM.
//   - PKI mount `pki/`, role `soul-seed` (allow_localhost+allow_any_name for
//     the test environment), root CA TTL 87600h.
//   - Sigil signing-key is NOT pre-seeded: KeyService introduces the key at runtime.

const (
	// vaultTokenHeader - Vault-token http header (same for CLI and API).
	vaultTokenHeader = "X-Vault-Token"

	// vaultJWTKeyPath - KV v2 data-path for the JWT signing-key (see `dev/keeper.dev.yml::auth.jwt.signing_key_ref`).
	// KVv2 requires a `data/` prefix between the mount and the logical path.
	vaultJWTKeyPath = "secret/data/keeper/jwt-signing-key"

	// sigilSigningKeyRef - config reference `sigil.signing_key_ref` in keeper.yml
	// (parity dev/keeper.dev.yml); vaultSigilKeyPath - its KV v2 data-path.
	sigilSigningKeyRef = "vault:secret/keeper/sigil-signing-key"
	vaultSigilKeyPath  = "secret/data/keeper/sigil-signing-key"

	// vaultPKIMountPath - endpoint to enable the PKI mount at path `pki/`.
	vaultPKIMountPath = "sys/mounts/pki"

	// vaultPKIRootGenPath - generate-internal root CA.
	vaultPKIRootGenPath = "pki/root/generate/internal"

	// vaultPKIRoleSoulSeed - role for issuing SoulSeed certs (symmetric with provision.sh step 5).
	vaultPKIRoleSoulSeed = "pki/roles/soul-seed"

	// vaultPKIIssueSoulSeed - endpoint to issue a leaf cert by role.
	vaultPKIIssueSoulSeed = "pki/issue/soul-seed"

	// hs256MinBytes - minimum HS256 key length (see keeper/internal/jwt/issuer.go::minSigningKeyBytes).
	hs256MinBytes = 32

	// vaultHTTPTimeout - deadline for a single Vault HTTP call.
	vaultHTTPTimeout = 10 * time.Second
)

// vaultClient - minimal HTTP client over the Vault dev API.
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

// write does a POST to `<addr>/v1/<path>` with a JSON body; returns the decoded
// `data` field of the response (for most write endpoints Vault puts the payload in `data`).
// status 204 (No Content, normal for config-mutate writes) -> (nil, nil).
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

// InitVaultTestSecrets sets up in Vault the minimal set of test secrets that
// Keeper needs to start in an L3b scenario:
//   - PKI mount + root CA + role `soul-seed`;
//   - JWT signing-key (HS256 32B base64) at `secret/keeper/jwt-signing-key`.
//
// `sigil-signing-key` is NOT pre-seeded - KeyService will introduce the key
// into the Sigil registry itself at startup via its API (see ADR-039 Amendment §3).
//
// L3b difference from L3a: the PKI role `soul-seed` will also be used to
// issue a cert to the real soul agent via the CSR-Bootstrap flow (L3b-2), not
// only for harness-side mTLS.
func InitVaultTestSecrets(t *testing.T, stack *Stack) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("InitVaultTestSecrets: stack.VaultAddr / vaultToken empty (NewStack not called?)")
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

	// Root CA (CN=soul-stack-test-ca - namespace isolated from the dev stack `soul-stack`).
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

	// JWT signing-key: 32 random bytes in base64.
	jwtKey := generateHS256Key(t)
	if _, err := vc.write(ctx, vaultJWTKeyPath, map[string]any{
		"data": map[string]any{
			"signing_key": jwtKey,
		},
	}); err != nil {
		t.Fatalf("InitVaultTestSecrets: write jwt signing-key: %v", err)
	}
}

// SeedSigilSigningKey puts the Sigil signing ed25519 key (ADR-026) into Vault
// KV `secret/keeper/sigil-signing-key`, field `signing_key` - base64 of a raw
// 32-byte seed (one of the forms in keeper/internal/sigil/key.go::parseEd25519Key).
//
// Called by NewStack ONLY when cfg.SoulModules is non-empty: a sigil block in
// keeper.yml without this secret fails `keeper run` at setupSigil (the
// cfg-fallback in buildSigilSigner reads the key at startup). The ADR-039 §3
// canon "sigil-key is NOT pre-seeded" still holds for stands without the
// plugin channel - InitVaultTestSecrets still doesn't seed this key.
func SeedSigilSigningKey(t *testing.T, stack *Stack) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("SeedSigilSigningKey: stack.VaultAddr / vaultToken empty (NewStack not called?)")
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

// SeedVaultKV puts an arbitrary KV v2 secret at logical path `secret/<rel>`
// with the given fields. Used by services that pull a secret keeper-side via
// CEL `vault('secret/<rel>#<field>')` in the render phase (redis-create:
// `vault('secret/redis/'+incarnation.name+'#password')`, ADR-010/ADR-012 - the
// password arrives on the host as a value, the Soul vault client doesn't pull it).
//
// rel - path WITHOUT the mount prefix and WITHOUT `data/` (the KV v2 infixes
// are added here): for the CEL path `secret/redis/<inc>` pass rel =
// "redis/<inc>". Keeper-side ReadKV normalizes the logical path to KVv2-relative
// and reads `secret/data/<rel>`, so the harness writes to the same place.
func SeedVaultKV(t *testing.T, stack *Stack, rel string, fields map[string]any) {
	t.Helper()
	if stack == nil || stack.VaultAddr == "" || stack.vaultToken == "" {
		t.Fatal("SeedVaultKV: stack.VaultAddr / vaultToken empty (NewStack not called?)")
	}
	vc := newVaultClient(stack.VaultAddr, stack.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), vaultHTTPTimeout)
	defer cancel()
	if _, err := vc.write(ctx, "secret/data/"+rel, map[string]any{"data": fields}); err != nil {
		t.Fatalf("SeedVaultKV(%q): %v", rel, err)
	}
}

// generateHS256Key returns 32 random bytes as a base64-encoded string.
// Symmetric with `openssl rand -base64 32` in `dev/provision.sh:90`.
func generateHS256Key(t *testing.T) string {
	t.Helper()
	b := make([]byte, hs256MinBytes)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generateHS256Key: rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// IssueKeeperServerCert issues a leaf cert via Vault PKI for the Keeper server
// TLS listeners (bootstrap + event_stream). SAN: 127.0.0.1 + localhost +
// host.docker.internal (the latter for connecting from an L3b soul container to
// keeper, which listens on the host), TTL 24h.
//
// Bootstrap is server-only TLS (ADR-012): the soul verifies the keeper cert by
// the keeper endpoint hostname. So SAN MUST cover the host the soul actually
// dials (keeperEndpointHost()). With an E2E_KEEPER_HOST override (WSL2: real
// host IP) - add it to ip_sans/alt_names, otherwise the soul gets a
// TLS-SAN-mismatch instead of connection-refused.
//
// Symmetric with `dev/provision.sh:155-172`. cert/key/caBundle are returned as
// PEM bytes for writing directly to disk before starting the keeper process.
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

// stringField extracts a field as []byte (PEM block).
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
