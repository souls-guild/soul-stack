package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperWithVault собирает минимально-валидный keeper.yml, подставляя заданный
// vault-блок (с уже выставленным отступом в 2 пробела на каждую строку).
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
	// Блок без `auth.method` вовсе — forward-compat: трактуется как token.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("token-режим без auth-блока должен быть валиден")
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
		t.Fatalf("method=token должен быть валиден")
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
		t.Fatalf("approle + role_id + secret_id_file должен быть валиден")
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
		t.Fatalf("approle + secret_id_env должен быть валиден")
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
		t.Fatalf("ожидался missing_required_field на role_id")
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
		t.Fatalf("ожидался missing_required_field на отсутствие secret_id источника")
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
		t.Fatalf("ожидался vault_auth_conflicting_secret_source")
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
		t.Fatalf("ожидался path_not_absolute на secret_id_file")
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
		t.Fatalf("ожидался vault_auth_conflicting_method (token при approle)")
	}
}

func TestVaultAuth_InvalidMethod(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth: { method: kerberos }`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "enum_invalid") {
		dump(t, diags)
		t.Fatalf("ожидался enum_invalid на неизвестный method")
	}
}

func TestVaultAuth_Token_UnusedAppRoleFields_Warn(t *testing.T) {
	// approle-поля при token-методе → warning, не error.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"
  auth:
    method: token
    role_id: stray`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("token + лишние approle-поля не должны давать error")
	}
	if !hasCode(diags, "vault_auth_unused_fields") {
		dump(t, diags)
		t.Fatalf("ожидался warning vault_auth_unused_fields")
	}
}

func TestVaultKVVersion_Empty_AutoDetect_OK(t *testing.T) {
	// Без kv_version — auto (probe на runtime). Старые конфиги работают.
	src := keeperWithVault(`  addr: "https://v:8200"
  token: "root"`)
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("kv_version опущен должен быть валиден (auto-detect)")
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
				t.Fatalf("kv_version=%q должен быть валиден", v)
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
		t.Fatalf("ожидался vault_kv_version_invalid на kv_version=3")
	}
}

// Секрет (значение secret_id) в принципе не задаётся в keeper.yml — только
// путь/имя env. Проверяем, что схема не имеет поля для plaintext secret_id:
// попытка задать его ловится как unknown_key, а не молча принимается.
func TestVaultAuth_NoPlaintextSecretIDField(t *testing.T) {
	src := keeperWithVault(`  addr: "https://v:8200"
  auth:
    method: approle
    role_id: keeper-prod
    secret_id: PLAINTEXT-SECRET`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("plaintext secret_id-поле должно отвергаться как unknown_key")
	}
	// На всякий случай — значение секрета не должно фигурировать в сообщениях
	// (unknown_key эхает имя ключа, не значение).
	for _, d := range diags {
		if strings.Contains(d.Message, "PLAINTEXT-SECRET") {
			t.Errorf("diagnostic leaked secret value: %q", d.Message)
		}
	}
}
