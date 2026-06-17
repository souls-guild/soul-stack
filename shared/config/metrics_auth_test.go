package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperBaseWithMetricsAuth собирает минимально-валидный keeper.yml с
// произвольным телом блока metrics: для тестов валидации basic-auth.
func keeperBaseWithMetricsAuth(metricsBlock string) []byte {
	return []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
` + metricsBlock)
}

func TestMetricsBasicAuth_Valid(t *testing.T) {
	src := keeperBaseWithMetricsAuth(`metrics:
  auth:
    basic:
      enabled: true
      username: scrape
      password_ref: vault:secret/keeper/metrics-password
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid metrics.auth.basic")
	}
	if cfg.Metrics == nil || cfg.Metrics.Auth == nil || cfg.Metrics.Auth.Basic == nil {
		t.Fatal("metrics.auth.basic not parsed")
	}
	b := cfg.Metrics.Auth.Basic
	if !b.Enabled || b.Username != "scrape" || b.PasswordRef != "vault:secret/keeper/metrics-password" {
		t.Errorf("parsed basic-auth = %+v, unexpected", b)
	}
}

func TestMetricsBasicAuth_EnabledMissingUsername(t *testing.T) {
	src := keeperBaseWithMetricsAuth(`metrics:
  auth:
    basic:
      enabled: true
      password_ref: vault:secret/keeper/metrics-password
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.metrics.auth.basic.username") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for username")
	}
}

func TestMetricsBasicAuth_EnabledMissingPasswordRef(t *testing.T) {
	src := keeperBaseWithMetricsAuth(`metrics:
  auth:
    basic:
      enabled: true
      username: scrape
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.metrics.auth.basic.password_ref") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for password_ref")
	}
}

func TestMetricsBasicAuth_PasswordRefNotVaultRef(t *testing.T) {
	// Plaintext-пароль вместо vault-ref запрещён.
	src := keeperBaseWithMetricsAuth(`metrics:
  auth:
    basic:
      enabled: true
      username: scrape
      password_ref: hunter2
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "vault_ref_invalid") {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for plaintext password_ref")
	}
}

func TestMetricsBasicAuth_DisabledNoFieldsOK(t *testing.T) {
	src := keeperBaseWithMetricsAuth(`metrics:
  auth:
    basic:
      enabled: false
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for disabled basic-auth with no fields")
	}
}
