//go:build e2e_k8s

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

// vault.go - minimal HTTP client for seeding Vault (PKI mount + JWT signing key +
// PG DSN) and issuing the keeper-server TLS cert. Symmetric to tests/e2e-live/harness/
// vault.go - the harness cannot import `keeper/internal/vault` (Go internal
// rules), so it talks to the Vault dev API over plain HTTP.
//
// Vault access in kind goes through port-forward (`kubectl port-forward
// svc/vault 0:8200`) - the Vault service is in-cluster, seeding happens host-side.

const (
	vaultRootToken        = "roottoken" // matches helm-values/vault.yaml::server.dev.devRootToken
	vaultPKIMountPath     = "sys/mounts/pki"
	vaultPKIRootGenPath   = "pki/root/generate/internal"
	vaultPKIRoleSoulSeed  = "pki/roles/soul-seed"
	vaultPKIIssueSoulSeed = "pki/issue/soul-seed"
	vaultJWTKeyPath       = "secret/data/keeper/jwt-signing-key"
	vaultPostgresDSNPath  = "secret/data/keeper/postgres"
	vaultHTTPTimeout      = 15 * time.Second
	hs256MinBytes         = 32
)

// vaultClient - HTTP client wrapper over the Vault dev API.
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

// write - POST `<addr>/v1/<path>` with a JSON body. 204 (config-mutate) -> nil/nil.
// 4xx/5xx -> error with body. Otherwise decodes the `data` field of the response.
func (vc *vaultClient) write(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("vault write %s: marshal: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vc.addr+"/v1/"+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("vault write %s: build request: %w", path, err)
	}
	req.Header.Set("X-Vault-Token", vc.token)
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
		return nil, fmt.Errorf("vault write %s: decode: %w", path, err)
	}
	if data, ok := out["data"].(map[string]any); ok {
		return data, nil
	}
	return out, nil
}

// waitVaultReady polls Vault `/v1/sys/health` until 200 (or 429 for standby/
// uninitialized - doesn't happen in dev-mode, but included for robustness).
// Timeout 60s - Vault dev-mode usually starts in <5s, but the first packet
// after port-forward can get dropped.
func waitVaultReady(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(limit) {
		resp, err := http.Get(addr + "/v1/sys/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("vault not ready at %s within %v: %v", addr, deadline, lastErr)
}

// seedVaultSecrets sets up PKI + JWT signing key + Postgres DSN in Vault,
// issues the keeper-server TLS cert for the in-cluster DNS name.
//
// Returns cert / key / CA PEM bytes - the caller puts them into a k8s Secret
// mounted into the keeper-pod.
func seedVaultSecrets(t *testing.T, vaultAddr, pgDSN string) (certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	vc := newVaultClient(vaultAddr, vaultRootToken)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// PKI mount.
	if _, err := vc.write(ctx, vaultPKIMountPath, map[string]any{
		"type": "pki",
		"config": map[string]any{
			"max_lease_ttl": "87600h",
		},
	}); err != nil {
		t.Fatalf("vault: enable pki mount: %v", err)
	}

	// Root CA.
	if _, err := vc.write(ctx, vaultPKIRootGenPath, map[string]any{
		"common_name": "soul-stack-l3c-ca",
		"ttl":         "87600h",
	}); err != nil {
		t.Fatalf("vault: generate root CA: %v", err)
	}

	// Role soul-seed: allow_any_name for the test environment (CN=keeper, alt names
	// on in-cluster service DNS).
	if _, err := vc.write(ctx, vaultPKIRoleSoulSeed, map[string]any{
		"allowed_domains":  "cluster.local,svc,default,localhost,example.com",
		"allow_subdomains": true,
		"allow_any_name":   true,
		"allow_localhost":  true,
		"allow_ip_sans":    true,
		"max_ttl":          "720h",
	}); err != nil {
		t.Fatalf("vault: create role soul-seed: %v", err)
	}

	// JWT signing key (HS256 32B base64, symmetric to provision.sh).
	jwtKey, err := generateHS256Key()
	if err != nil {
		t.Fatalf("vault: generate HS256 key: %v", err)
	}
	if _, err := vc.write(ctx, vaultJWTKeyPath, map[string]any{
		"data": map[string]any{
			"signing_key": jwtKey,
		},
	}); err != nil {
		t.Fatalf("vault: write JWT signing-key: %v", err)
	}

	// PG DSN - keeper.yml::postgres.dsn_ref refers to this KV.
	if _, err := vc.write(ctx, vaultPostgresDSNPath, map[string]any{
		"data": map[string]any{
			"dsn": pgDSN,
		},
	}); err != nil {
		t.Fatalf("vault: write postgres DSN: %v", err)
	}

	// Issue the keeper-server TLS leaf cert. CN=keeper + alt names on in-cluster
	// service DNS (`keeper`, `keeper.default.svc.cluster.local`). IP-SAN:
	// 127.0.0.1 (host-side port-forward tail) + 10.0.0.0/8 is covered by
	// allow_any_name=true / allow_ip_sans=true.
	data, err := vc.write(ctx, vaultPKIIssueSoulSeed, map[string]any{
		"common_name": "keeper",
		"alt_names":   "keeper,keeper.default.svc,keeper.default.svc.cluster.local,localhost",
		"ip_sans":     "127.0.0.1",
		"ttl":         "24h",
	})
	if err != nil {
		t.Fatalf("vault: issue keeper-server cert: %v", err)
	}
	certPEM = pemField(t, data, "certificate")
	keyPEM = pemField(t, data, "private_key")
	caPEM = pemField(t, data, "issuing_ca")
	return
}

func generateHS256Key() (string, error) {
	b := make([]byte, hs256MinBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func pemField(t *testing.T, data map[string]any, field string) []byte {
	t.Helper()
	v, ok := data[field]
	if !ok {
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		t.Fatalf("vault response missing field %q (got keys=%v)", field, keys)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("vault response field %q has type %T, want string", field, v)
	}
	return []byte(s)
}
