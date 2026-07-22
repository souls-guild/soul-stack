package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperWithVault assembles a minimally valid keeper.yml, injecting the given
// vault block (already indented 2 spaces per line).
func keeperWithVault(vaultBlock string) []byte {
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
` + vaultBlock + `
  pki_mount: pki/x
`)
}

func TestVaultAuth_Default_IsToken(t *testing.T) {
	// A block without `auth.method` at all — forward-compat: treated as token.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("token mode without an auth block should be valid")
	}
	if got := cfg.Vault.Auth.ResolvedAuthMethod(); got != AuthMethodToken {
		t.Errorf("ResolvedAuthMethod = %q, want token", got)
	}
}

func TestVaultAuth_ExplicitToken_OK(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  auth: { method: token }`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("method=token should be valid")
	}
}

func TestVaultAuth_AppRole_FileSource_OK(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("approle + role_id + secret_id_file should be valid")
	}
	if cfg.Vault.Auth.RoleID != "keeper-prod" {
		t.Errorf("RoleID = %q", cfg.Vault.Auth.RoleID)
	}
	if cfg.Vault.Auth.SecretIDFile != "/etc/keeper/vault-secret-id" {
		t.Errorf("SecretIDFile = %q", cfg.Vault.Auth.SecretIDFile)
	}
}

func TestVaultAuth_AppRole_EnvSource_OK(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_env: KEEPER_VAULT_SECRET_ID`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("approle + secret_id_env should be valid")
	}
}

func TestVaultAuth_AppRole_MissingRoleID(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    secret_id_file: /etc/keeper/vault-secret-id`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.vault.auth.role_id") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on role_id")
	}
}

func TestVaultAuth_AppRole_MissingSecretSource(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for missing secret_id source")
	}
}

func TestVaultAuth_AppRole_ConflictingSecretSources(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id
    secret_id_env: KEEPER_VAULT_SECRET_ID`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "vault_auth_conflicting_secret_source") {
		dump(t, diags)
		t.Fatalf("expected vault_auth_conflicting_secret_source")
	}
}

func TestVaultAuth_AppRole_SecretIDFileNotAbsolute(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: relative/secret-id`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "path_not_absolute", "$.vault.auth.secret_id_file") {
		dump(t, diags)
		t.Fatalf("expected path_not_absolute on secret_id_file")
	}
}

func TestVaultAuth_AppRole_TokenAlsoSet_Conflict(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "vault_auth_conflicting_method") {
		dump(t, diags)
		t.Fatalf("expected vault_auth_conflicting_method (token alongside approle)")
	}
}

func TestVaultAuth_InvalidMethod(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth: { method: kerberos }`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "enum_invalid") {
		dump(t, diags)
		t.Fatalf("expected enum_invalid on an unknown method")
	}
}

func TestVaultAuth_Token_UnusedAppRoleFields_Warn(t *testing.T) {
	// approle fields with the token method → warning, not error.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  auth:
    method: token
    role_id: stray`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("token + extra approle fields should not produce an error")
	}
	if !hasCode(diags, "vault_auth_unused_fields") {
		dump(t, diags)
		t.Fatalf("expected warning vault_auth_unused_fields")
	}
}

func TestVaultKVVersion_Empty_AutoDetect_OK(t *testing.T) {
	// Without kv_version — auto (probe at runtime). Old configs keep working.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("kv_version omitted should be valid (auto-detect)")
	}
	if cfg.Vault.KVVersion != "" {
		t.Errorf("KVVersion = %q, want empty", cfg.Vault.KVVersion)
	}
}

func TestVaultKVVersion_Explicit_OK(t *testing.T) {
	for _, v := range []string{"1", "2"} {
		v := v
		t.Run("v"+v, func(t *testing.T) {
			src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  kv_version: "` + v + `"`)
			cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("kv_version=%q should be valid", v)
			}
			if cfg.Vault.KVVersion != v {
				t.Errorf("KVVersion = %q, want %q", cfg.Vault.KVVersion, v)
			}
		})
	}
}

func TestVaultKVVersion_Invalid_Rejected(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  kv_version: "3"`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_kv_version_invalid", "$.vault.kv_version") {
		dump(t, diags)
		t.Fatalf("expected vault_kv_version_invalid on kv_version=3")
	}
}

// The secret (secret_id value) is never set in keeper.yml — only the path/env
// name. We check that the schema has no field for a plaintext secret_id: an
// attempt to set it is caught as unknown_key, not silently accepted.
func TestVaultAuth_NoPlaintextSecretIDField(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id: PLAINTEXT-SECRET`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("plaintext secret_id field should be rejected as unknown_key")
	}
	// Just in case — the secret value must not appear in messages
	// (unknown_key echoes the key name, not the value).
	for _, d := range diags {
		if strings.Contains(d.Message, "PLAINTEXT-SECRET") {
			t.Errorf("diagnostic leaked secret value: %q", d.Message)
		}
	}
}
