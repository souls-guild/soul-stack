//go:build integration

// Integration-тесты Vault-клиента через testcontainers-go.
//
// Поднимают hashicorp/vault в dev-режиме (root-token=root) на эфемерном
// порту, прогоняют write+read round-trip через vault/api напрямую и
// сверяют с тем, что отдаёт наш Client.
//
// Запуск:
//
//	make test-integration
//	# или
//	cd keeper && go test -tags=integration -race -count=1 ./internal/vault/
//
// Паттерн совпадает с auditpg/integration_test.go: TestMain → run() →
// контейнер per-package, тесты используют общий integrationClient.
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
	// integrationToken — root-токен dev-режима, совпадает с dev/docker-compose.yml.
	integrationToken = "root"
	// integrationImage — version pin, совпадает с dev/docker-compose.yml.
	integrationImage = "hashicorp/vault:1.18"
)

// integrationClient — наш Client поверх testcontainer-Vault-а.
var integrationClient *Client

// integrationAPI — низкоуровневый vault/api клиент для write-операций в
// тестах (наш Client read-only).
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

	// Низкоуровневый клиент для seed-а.
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

	// PKI backend под `pki/` mount + role `soul-seed`. Зеркалит
	// docs/dev/local-setup.md (раздел Vault PKI). Если provisioning
	// падает — PKI-тесты будут сами skip-ить через `pkiReady`.
	if err := provisionPKI(ctx, integrationAPI); err != nil {
		log.Printf("vault integration: provisionPKI failed (PKI tests will be skipped): %v", err)
	} else {
		pkiReady = true
	}

	return m.Run()
}

// pkiReady — set provisionPKI на true, если PKI backend поднят. Тесты
// PKI используют его, чтобы skip-ить при ошибке provisioning-а
// (например, на CI без сети, где Vault images не имеют PKI plugin-а).
var pkiReady bool

// provisionPKI поднимает PKI secrets engine по path `pki/`, генерирует
// root cert и создаёт role `soul-seed`. Симметрично командам в
// docs/dev/local-setup.md → Vault PKI.
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

// --- KV v1/v2 прозрачность (ADR-017(b) amendment 2026-06-22) -------------
//
// Главный guard-набор: probe-механизм читает ОБЕ версии KV прозрачно. Прежний
// «угадывание по классу ошибки KVv2.Get» отвергнут — обычный v1-секрет был
// неотличим от v2-missing (ErrSecretNotFound) и НИКОГДА не читался.

// mountV1 поднимает дополнительный KV v1 mount `kv-v1` (один раз) и
// возвращает наш Client, нацеленный на него. v2-mount `secret/` поднят
// dev-режимом по умолчанию.
func newV1MountClient(ctx context.Context, t *testing.T) *Client {
	t.Helper()
	const mount = "kv-v1"
	// Mount идемпотентен в пределах прогона: повторный Mount даст ошибку
	// "path is already in use" — её игнорируем (mount уже есть).
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

// TestReadKV_V1Mount — главный guard: обычный секрет на KV v1 читается. Ровно
// то, что прежний дизайн НИКОГДА не читал (v1-секрет ≡ v2-missing для эвристики).
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
	// Logical-форма пути — тот же результат.
	got2, err := cl.ReadKV(ctx, "kv-v1/redis/admin")
	if err != nil {
		t.Fatalf("ReadKV v1 (logical): %v", err)
	}
	if got2["password"] != "v1-secret" {
		t.Errorf("v1 logical payload = %v", got2)
	}
}

// TestWriteKV_V1Mount — главный write-guard: запись на KV v1 ложится end-to-end
// и читается обратно плоско. До этого v1-запись была доказана только unit-
// роутингом (TestWriteKV_V1Routing глушит результат на mock-е); это реальный
// путь записи приватника Sigil на v1-mount (KVv1.Put, плоский путь без /data/).
func TestWriteKV_V1Mount(t *testing.T) {
	ctx := context.Background()
	cl := newV1MountClient(ctx, t)

	want := map[string]any{"signing_key": "v1-write-secret", "key_id": "sigil-01"}
	if err := cl.WriteKV(ctx, "sigil-keys/written-v1", want); err != nil {
		t.Fatalf("WriteKV v1: %v", err)
	}

	// Сверка низкоуровневым клиентом — то, что реально легло на v1-mount.
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

	// И через наш ReadKV (round-trip целиком через клиент): плоский payload.
	back, err := cl.ReadKV(ctx, "sigil-keys/written-v1")
	if err != nil {
		t.Fatalf("ReadKV after WriteKV v1: %v", err)
	}
	if back["signing_key"] != "v1-write-secret" {
		t.Errorf("v1 read-back payload = %v", back)
	}
}

// TestVaultCEL_HashField_V1Mount — `#field`-селектор vault('kv-v1/x#field')
// поверх РЕАЛЬНОГО v1-mount-а: путь splitVaultField → ReadKV (v1-routing) →
// извлечение одного поля. До этого #field был покрыт только на v2 (version-
// agnostic stubKV в shared/cel не моделирует routing). Здесь KVReader — наш
// настоящий v1-Client, поэтому проверка закрывает v1-ветку end-to-end.
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

	// #field → одно поле напрямую.
	out, err := eng.EvalInterpolation("${ vault('kv-v1/redis/hashfield#password') }", cel.Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("EvalInterpolation #field v1: %v", err)
	}
	if out != "v1-hashed-secret" {
		t.Errorf("vault(#field) on v1 = %v, want v1-hashed-secret", out)
	}

	// Без #field — весь map, доступ к полю CEL-выражением (та же v1-ветка).
	out2, err := eng.EvalInterpolation("${ vault('kv-v1/redis/hashfield').user }", cel.Vars{Ctx: ctx})
	if err != nil {
		t.Fatalf("EvalInterpolation .field v1: %v", err)
	}
	if out2 != "redis" {
		t.Errorf("vault().user on v1 = %v, want redis", out2)
	}
}

// TestReadKV_V2Mount — регресс-guard: v2-mount продолжает читаться через probe.
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

// TestReadKV_V2Missing_NotMaskedAsV1 — инвариант держится КОНСТРУКТИВНО:
// missing-секрет на v2 → ErrVaultKVNotFound (а не «деградация в v1-чтение»),
// потому что версию даёт probe sys/internal/ui/mounts, а не класс ошибки
// KVv2.Get. Именно этот случай ломал прежнюю эвристику.
func TestReadKV_V2Missing_NotMaskedAsV1(t *testing.T) {
	_, err := integrationClient.ReadKV(context.Background(), "keeper/definitely-missing-v2")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("v2 missing: err=%v, want ErrVaultKVNotFound (probe резолвит v2 конструктивно)", err)
	}
}

// TestReadKV_ExplicitVersionOverride — kv_version="1" форсит v1-роутинг без
// probe. Для жёсткой проверки «probe не вызван» поднимаем Client на mount, у
// которого probe-endpoint вернул бы v2 (secret/), но читать его как v1 нельзя —
// поэтому override проверяем на реальном v1-mount с принудительным kv_version.
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

	// Relative форма пути — тот же результат.
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

// TestIntegration_VaultListKV — LIST под prefix отдаёт имена секретов
// (последний сегмент, key_id), а не полные пути.
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
	// Logical-форма prefix-а даёт тот же результат.
	got2, err := integrationClient.ListKV(ctx, "secret/keeper/sigil-keys")
	if err != nil {
		t.Fatalf("ListKV (logical prefix): %v", err)
	}
	if len(got2) != len(got) {
		t.Errorf("logical-prefix ListKV returned %d names, relative returned %d", len(got2), len(got))
	}
}

// TestIntegration_VaultListKV_EmptyPrefix — несуществующая подпапка → (nil, nil),
// НЕ ошибка (валидное «сирот нет»).
func TestIntegration_VaultListKV_EmptyPrefix(t *testing.T) {
	got, err := integrationClient.ListKV(context.Background(), "keeper/never-existed-prefix")
	if err != nil {
		t.Fatalf("ListKV on missing prefix should not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("ListKV on missing prefix should return nil, got %v", got)
	}
}

// TestIntegration_VaultReadKVMetadata — metadata-read отдаёт created_time, не
// трогая data-путь секрета.
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

// TestIntegration_VaultReadKVMetadata_NotFound — несуществующий путь →
// ErrVaultKVNotFound.
func TestIntegration_VaultReadKVMetadata_NotFound(t *testing.T) {
	_, err := integrationClient.ReadKVMetadata(context.Background(), "keeper/sigil-keys/never-existed")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("ReadKVMetadata: err=%v, want errors.Is(ErrVaultKVNotFound)", err)
	}
}

// TestIntegration_PKI_SignCSR — happy-path: CSR на test.example.com →
// получили валидный PEM-cert + CA-chain + serial_number + not_after.
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
	// Парсим cert — sanity: CN совпадает с CSR.
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

// TestIntegration_PKI_BadRole — несуществующий role в PKI mount возвращает
// ошибку (Vault → 404 + errors).
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

// TestIntegration_PKI_DomainNotAllowed — CN вне `allowed_domains` →
// Vault отвергает (cf. provisionPKI role-конфиг).
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

// mustMakeCSR генерирует RSA-2048-ключ и PEM-encoded CSR на указанный CN.
//
// RSA, а не ECDSA — Vault PKI role по умолчанию (`key_type: rsa`)
// отвергает не-RSA ключи. Опция `key_type=ec` в provisionPKI оставлена
// как future-proof, но не используется в MVP, чтобы зеркалить
// docs/dev/local-setup.md (где key_type не указан — Vault default).
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
