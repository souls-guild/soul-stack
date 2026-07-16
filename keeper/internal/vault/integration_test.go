//go:build integration

// Integration tests for Vault client via testcontainers-go.
// Runs hashicorp/vault in dev mode (root-token=root) on ephemeral port,
// performs write+read round-trip via vault/api, and verifies against Client.
//
// Run with:
//
//	make test-integration
//	# or
//	cd keeper && go test -tags=integration -race -count=1 ./internal/vault/
//
// Pattern matches auditpg/integration_test.go: TestMain → run() →
// per-package container, tests use shared integrationClient.
package vault

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	// integrationToken is dev-mode root token, matches dev/docker-compose.yml.
	integrationToken = "root"
	// integrationImage is pinned version matching dev/docker-compose.yml.
	integrationImage = "hashicorp/vault:1.18"
)

// integrationClient is our Client wrapping testcontainer Vault.
var integrationClient *Client

// integrationAPI is low-level vault/api client for write operations in tests
// (our Client is read-only).
var integrationAPI *vaultapi.Client

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcvault.Run(ctx, integrationImage, tcvault.WithToken(integrationToken))
	if err != nil {
		if requireDocker() {
			log.Fatalf("vault integration: setup failed (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER set): %v", err)
		}
		log.Printf("vault integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	addr, err := ctr.HttpHostAddress(ctx)
	if err != nil {
		log.Printf("vault integration: HttpHostAddress: %v", err)
		return 1
	}

	// Low-level client for seeding.
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = addr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		log.Printf("vault integration: vaultapi.NewClient: %v", err)
		return 1
	}
	api.SetToken(integrationToken)
	integrationAPI = api

	cl, err := NewClient(ctx, config.KeeperVault{
		Addr:    addr,
		Token:   integrationToken,
		KVMount: "secret",
	})
	if err != nil {
		log.Printf("vault integration: NewClient: %v", err)
		return 1
	}
	integrationClient = cl

	// PKI backend at `pki/` mount + role `soul-seed`. Mirrors
	// docs/dev/local-setup.md (Vault PKI section). If provisioning fails,
	// PKI tests will skip via `pkiReady`.
	if err := provisionPKI(ctx, integrationAPI); err != nil {
		log.Printf("vault integration: provisionPKI failed (PKI tests will be skipped): %v", err)
	} else {
		pkiReady = true
	}

	return m.Run()
}

// pkiReady is set by provisionPKI if PKI backend is up. PKI tests check it to
// skip on provisioning error (e.g., CI without network, Vault image lacks PKI).
var pkiReady bool

// provisionPKI sets up PKI secrets engine at `pki/`, generates root cert,
// and creates role `soul-seed`. Mirrors commands in docs/dev/local-setup.md.
func provisionPKI(ctx context.Context, api *vaultapi.Client) error {
	if err := api.Sys().Mount("pki", &vaultapi.MountInput{
		Type: "pki",
		Config: vaultapi.MountConfigInput{
			MaxLeaseTTL: "87600h",
		},
	}); err != nil {
		return fmt.Errorf("mount pki: %w", err)
	}
	if _, err := api.Logical().WriteWithContext(ctx, "pki/root/generate/internal", map[string]any{
		"common_name": "soul-stack-test",
		"ttl":         "87600h",
	}); err != nil {
		return fmt.Errorf("generate root: %w", err)
	}
	if _, err := api.Logical().WriteWithContext(ctx, "pki/roles/soul-seed", map[string]any{
		"allowed_domains":  "example.com,test,localhost",
		"allow_subdomains": true,
		"allow_localhost":  true,
		"max_ttl":          "720h",
	}); err != nil {
		return fmt.Errorf("create role: %w", err)
	}
	return nil
}

// --- KV v1/v2 transparency (ADR-017(b) amendment 2026-06-22) -----------
//
// Guard suite: probe mechanism transparently reads both KV versions.
// Previous "guess by KVv2.Get error class" rejected — plain v1 secret was
// indistinguishable from v2-missing (ErrSecretNotFound) and never read.

// newV1MountClient sets up additional KV v1 mount `kv-v1` (once) and returns
// Client targeting it. v2-mount `secret/` is up by default from dev mode.
func newV1MountClient(ctx context.Context, t *testing.T) *Client {
	t.Helper()
	const mount = "kv-v1"
	// Mount is idempotent: retry gives "path is already in use" which we ignore.
	err := integrationAPI.Sys().Mount(mount, &vaultapi.MountInput{
		Type:    "kv",
		Options: map[string]string{"version": "1"},
	})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("mount %s: %v", mount, err)
	}

	cl, err := NewClient(ctx, config.KeeperVault{
		Addr:    integrationClient.c.Address(),
		Token:   integrationToken,
		KVMount: mount,
	})
	if err != nil {
		t.Fatalf("NewClient(kv-v1): %v", err)
	}
	return cl
}

// TestReadKV_V1Mount is the key guard: plain secret on KV v1 is read.
// This is exactly what old design never read (v1-secret ≡ v2-missing).
func TestReadKV_V1Mount(t *testing.T) {
	ctx := context.Background()
	cl := newV1MountClient(ctx, t)

	want := map[string]any{"password": "v1-secret", "user": "redis"}
	if err := integrationAPI.KVv1("kv-v1").Put(ctx, "redis/admin", want); err != nil {
		t.Fatalf("seed KVv1 Put: %v", err)
	}

	got, err := cl.ReadKV(ctx, "redis/admin")
	if err != nil {
		t.Fatalf("ReadKV v1: %v", err)
	}
	if got["password"] != "v1-secret" || got["user"] != "redis" {
		t.Errorf("v1 payload = %v, want flat %v", got, want)
	}
	// Logical path form gives same result.
	got2, err := cl.ReadKV(ctx, "kv-v1/redis/admin")
	if err != nil {
		t.Fatalf("ReadKV v1 (logical): %v", err)
	}
	if got2["password"] != "v1-secret" {
		t.Errorf("v1 logical payload = %v", got2)
	}
}

// TestWriteKV_V1Mount is the key write guard: writes to KV v1 work end-to-end
// and read back flat. Previously v1-write was proven only by unit routing
// (TestWriteKV_V1Routing mocks result); this is real write path for Sigil
// private key to v1-mount (KVv1.Put, flat path without /data/).
func TestWriteKV_V1Mount(t *testing.T) {
	ctx := context.Background()
	cl := newV1MountClient(ctx, t)

	want := map[string]any{"signing_key": "v1-write-secret", "key_id": "sigil-01"}
	if err := cl.WriteKV(ctx, "sigil-keys/written-v1", want); err != nil {
		t.Fatalf("WriteKV v1: %v", err)
	}

	// Verify with low-level client what actually landed on v1-mount.
	got, err := integrationAPI.KVv1("kv-v1").Get(ctx, "sigil-keys/written-v1")
	if err != nil {
		t.Fatalf("low-level KVv1 Get: %v", err)
	}
	if got == nil || got.Data == nil {
		t.Fatal("KVv1 Get: nil secret after WriteKV")
	}
	if got.Data["signing_key"] != "v1-write-secret" || got.Data["key_id"] != "sigil-01" {
		t.Errorf("v1 stored payload = %v, want %v", got.Data, want)
	}

	// And via our ReadKV (full round-trip through client): flat payload.
	back, err := cl.ReadKV(ctx, "sigil-keys/written-v1")
	if err != nil {
		t.Fatalf("ReadKV after WriteKV v1: %v", err)
	}
	if back["signing_key"] != "v1-write-secret" {
		t.Errorf("v1 read-back payload = %v", back)
	}
}

// TestVaultCEL_HashField_V1Mount tests `#field` selector vault('kv-v1/x#field')
// on REAL v1-mount: path splitVaultField → ReadKV (v1-routing) → field extract.
// Previously #field covered only v2 (version-agnostic stubKV in shared/cel
// doesn't model routing). Here KVReader is our real v1-Client, closing v1 e2e.
func TestVaultCEL_HashField_V1Mount(t *testing.T) {
	ctx := context.Background()
	cl := newV1MountClient(ctx, t)

	if err := integrationAPI.KVv1("kv-v1").Put(ctx, "redis/hashfield", map[string]any{
		"password": "v1-hashed-secret",
		"user":     "redis",
	}); err != nil {
		t.Fatalf("seed KVv1 Put: %v", err)
	}

	eng, err := cel.New(cel.WithVault(cl))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}

	// #field → single field directly.
	out, err := eng.EvalInterpolation("${ vault('kv-v1/redis/hashfield#password') }", cel.Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("EvalInterpolation #field v1: %v", err)
	}
	if out != "v1-hashed-secret" {
		t.Errorf("vault(#field) on v1 = %v, want v1-hashed-secret", out)
	}

	// Without #field — whole map, access via CEL expression (same v1 path).
	out2, err := eng.EvalInterpolation("${ vault('kv-v1/redis/hashfield').user }", cel.Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("EvalInterpolation .field v1: %v", err)
	}
	if out2 != "redis" {
		t.Errorf("vault().user on v1 = %v, want redis", out2)
	}
}

// TestReadKV_V2Mount is regression guard: v2-mount still reads via probe.
func TestReadKV_V2Mount(t *testing.T) {
	ctx := context.Background()
	want := map[string]any{"signing_key": "v2-secret"}
	if _, err := integrationAPI.KVv2("secret").Put(ctx, "keeper/v2probe", want); err != nil {
		t.Fatalf("seed KVv2 Put: %v", err)
	}
	got, err := integrationClient.ReadKV(ctx, "keeper/v2probe")
	if err != nil {
		t.Fatalf("ReadKV v2: %v", err)
	}
	if got["signing_key"] != "v2-secret" {
		t.Errorf("v2 payload = %v", got)
	}
}

// TestReadKV_V2Missing_NotMaskedAsV1 ensures invariant constructively:
// missing secret on v2 → ErrVaultKVNotFound (not "degrade to v1"), because
// version comes from probe sys/internal/ui/mounts, not KVv2.Get error class.
// This case broke the old heuristic.
func TestReadKV_V2Missing_NotMaskedAsV1(t *testing.T) {
	_, err := integrationClient.ReadKV(context.Background(), "keeper/definitely-missing-v2")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("v2 missing: err=%v, want ErrVaultKVNotFound (probe resolves v2 constructively)", err)
	}
}

// TestReadKV_ExplicitVersionOverride tests kv_version="1" forces v1 routing
// without probe. To strictly verify "probe not called", we use mount where
// probe would return v2 (secret/), but can't read as v1, so override is
// tested on real v1-mount with explicit kv_version.
func TestReadKV_ExplicitVersionOverride(t *testing.T) {
	ctx := context.Background()
	const mount = "kv-v1"
	err := integrationAPI.Sys().Mount(mount, &vaultapi.MountInput{
		Type: "kv", Options: map[string]string{"version": "1"},
	})
	if err != nil && !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("mount %s: %v", mount, err)
	}
	if err := integrationAPI.KVv1(mount).Put(ctx, "ov/x", map[string]any{"k": "ov"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cl, err := NewClient(ctx, config.KeeperVault{
		Addr: integrationClient.c.Address(), Token: integrationToken,
		KVMount: mount, KVVersion: "1",
	})
	if err != nil {
		t.Fatalf("NewClient override: %v", err)
	}
	got, err := cl.ReadKV(ctx, "ov/x")
	if err != nil {
		t.Fatalf("ReadKV override=1: %v", err)
	}
	if got["k"] != "ov" {
		t.Errorf("override payload = %v", got)
	}
}

func TestIntegration_VaultReadKV_RoundTrip(t *testing.T) {
	ctx := context.Background()
	kv := integrationAPI.KVv2("secret")

	want := map[string]any{
		"signing_key": "0123456789abcdef0123456789abcdef",
		"created_by":  "integration-test",
	}
	if _, err := kv.Put(ctx, "keeper/jwt-signing-key", want); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	got, err := integrationClient.ReadKV(ctx, "secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != want["signing_key"] {
		t.Errorf("signing_key = %v, want %v", got["signing_key"], want["signing_key"])
	}
	if got["created_by"] != want["created_by"] {
		t.Errorf("created_by = %v, want %v", got["created_by"], want["created_by"])
	}

	// Relative form of path gives same result.
	got2, err := integrationClient.ReadKV(ctx, "keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV (relative): %v", err)
	}
	if got2["signing_key"] != want["signing_key"] {
		t.Errorf("relative signing_key = %v", got2["signing_key"])
	}
}

func TestIntegration_VaultReadKV_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := integrationClient.ReadKV(ctx, "secret/keeper/never-existed")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("ReadKV: err=%v, want errors.Is(ErrVaultKVNotFound)", err)
	}
}

func TestIntegration_Ping(t *testing.T) {
	if err := integrationClient.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// TestIntegration_VaultListKV verifies LIST under prefix returns secret names
// (last segment, key_id), not full paths.
func TestIntegration_VaultListKV(t *testing.T) {
	ctx := context.Background()
	kv := integrationAPI.KVv2("secret")

	for _, id := range []string{"keya", "keyb", "keyc"} {
		if _, err := kv.Put(ctx, "keeper/sigil-keys/"+id, map[string]any{"signing_key": "x"}); err != nil {
			t.Fatalf("seed Put %s: %v", id, err)
		}
	}

	got, err := integrationClient.ListKV(ctx, "keeper/sigil-keys")
	if err != nil {
		t.Fatalf("ListKV: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, want := range []string{"keya", "keyb", "keyc"} {
		if !seen[want] {
			t.Errorf("ListKV result %v missing %q (expected last-segment key_id)", got, want)
		}
	}
	// Logical form of prefix gives same result.
	got2, err := integrationClient.ListKV(ctx, "secret/keeper/sigil-keys")
	if err != nil {
		t.Fatalf("ListKV (logical prefix): %v", err)
	}
	if len(got2) != len(got) {
		t.Errorf("logical-prefix ListKV returned %d names, relative returned %d", len(got2), len(got))
	}
}

// TestIntegration_VaultListKV_EmptyPrefix verifies nonexistent subfolder →
// (nil, nil), not error (valid "no orphans").
func TestIntegration_VaultListKV_EmptyPrefix(t *testing.T) {
	got, err := integrationClient.ListKV(context.Background(), "keeper/never-existed-prefix")
	if err != nil {
		t.Fatalf("ListKV on missing prefix should not error: %v", err)
	}
	if got != nil {
		t.Errorf("ListKV on missing prefix should return nil, got %v", got)
	}
}

// TestIntegration_VaultReadKVMetadata verifies metadata-read returns created_time
// without touching secret data path.
func TestIntegration_VaultReadKVMetadata(t *testing.T) {
	ctx := context.Background()
	kv := integrationAPI.KVv2("secret")

	before := time.Now().Add(-time.Minute)
	if _, err := kv.Put(ctx, "keeper/sigil-keys/meta-test", map[string]any{"signing_key": "x"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	after := time.Now().Add(time.Minute)

	created, err := integrationClient.ReadKVMetadata(ctx, "keeper/sigil-keys/meta-test")
	if err != nil {
		t.Fatalf("ReadKVMetadata: %v", err)
	}
	if created.Before(before) || created.After(after) {
		t.Errorf("created_time %v out of expected window [%v, %v]", created, before, after)
	}
}

// TestIntegration_VaultReadKVMetadata_NotFound verifies nonexistent path →
// ErrVaultKVNotFound.
func TestIntegration_VaultReadKVMetadata_NotFound(t *testing.T) {
	_, err := integrationClient.ReadKVMetadata(context.Background(), "keeper/sigil-keys/never-existed")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("ReadKVMetadata: err=%v, want errors.Is(ErrVaultKVNotFound)", err)
	}
}

// TestIntegration_PKI_SignCSR is happy-path: CSR for test.example.com →
// get valid PEM-cert + CA-chain + serial_number + not_after.
func TestIntegration_PKI_SignCSR(t *testing.T) {
	if !pkiReady {
		t.Skip("PKI backend not provisioned")
	}
	ctx := context.Background()
	csrPEM := mustMakeCSR(t, "test.example.com")

	res, err := integrationClient.SignCSR(ctx, "pki", "soul-seed", csrPEM)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	if !strings.Contains(string(res.CertificatePEM), "BEGIN CERTIFICATE") {
		t.Errorf("CertificatePEM not PEM-encoded: %q", res.CertificatePEM)
	}
	if !strings.Contains(string(res.CAChainPEM), "BEGIN CERTIFICATE") {
		t.Errorf("CAChainPEM not PEM-encoded: %q", res.CAChainPEM)
	}
	if res.SerialNumber == "" {
		t.Error("SerialNumber empty")
	}
	if res.NotAfter.IsZero() {
		t.Error("NotAfter zero")
	}
	if !res.NotAfter.After(time.Now()) {
		t.Errorf("NotAfter %v not in the future", res.NotAfter)
	}
	// Parse cert — sanity check: CN matches CSR.
	block, _ := pem.Decode(res.CertificatePEM)
	if block == nil {
		t.Fatal("pem.Decode: nil block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	if cert.Subject.CommonName != "test.example.com" {
		t.Errorf("Subject.CommonName = %q, want test.example.com", cert.Subject.CommonName)
	}
}

// TestIntegration_PKI_BadRole verifies nonexistent role in PKI mount
// returns error (Vault → 404 + errors).
func TestIntegration_PKI_BadRole(t *testing.T) {
	if !pkiReady {
		t.Skip("PKI backend not provisioned")
	}
	csrPEM := mustMakeCSR(t, "test.example.com")
	_, err := integrationClient.SignCSR(context.Background(), "pki", "no-such-role", csrPEM)
	if err == nil {
		t.Fatal("SignCSR with bad role: nil err, want failure")
	}
}

// TestIntegration_PKI_DomainNotAllowed verifies CN outside `allowed_domains` →
// Vault rejects (cf. provisionPKI role config).
func TestIntegration_PKI_DomainNotAllowed(t *testing.T) {
	if !pkiReady {
		t.Skip("PKI backend not provisioned")
	}
	csrPEM := mustMakeCSR(t, "evil.attacker.com")
	_, err := integrationClient.SignCSR(context.Background(), "pki", "soul-seed", csrPEM)
	if err == nil {
		t.Fatal("SignCSR with disallowed domain: nil err, want failure")
	}
}

// mustMakeCSR generates RSA-2048 key and PEM-encoded CSR for given CN.
// Uses RSA not ECDSA — Vault PKI role defaults to `key_type: rsa` and rejects
// non-RSA keys. Option `key_type=ec` in provisionPKI kept for future-proofing
// but unused in MVP to mirror docs/dev/local-setup.md (key_type unspecified).
func mustMakeCSR(t *testing.T, cn string) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: cn},
		DNSNames: []string{cn},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	var b strings.Builder
	if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}
	return b.String()
}
