package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ldapValidBlock — корректный блок auth.ldap (ldaps:// + search-bind), к которому
// тесты добавляют/меняют отдельные поля. Дописывается к keeperBaseRequired.
const ldapValidBlock = `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
    bind_mode: search
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
    base_dn: "ou=people,dc=example,dc=com"
    user_filter: "(uid=%s)"
    group_filter: "(member=%s)"
    group_attr: cn
    aid_attr: uid
    group_role_map:
      ops: ["cluster-admin"]
`

// TestKeeperAuthLDAP_Valid — корректный ldaps:// + search-bind грузится без
// ошибок (semantic-фаза auth.ldap).
func TestKeeperAuthLDAP_Valid(t *testing.T) {
	src := keeperBaseRequired + ldapValidBlock
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid auth.ldap block; got %d diags", len(diags))
	}
}

// TestKeeperAuthLDAP_PlaintextForbidden — ldap:// без start_tls запрещён
// (TLS-required, безопасность на первом месте, ADR-058(g)).
func TestKeeperAuthLDAP_PlaintextForbidden(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldap://ldap.example.com:389"
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "ldap_plaintext_forbidden") {
		dump(t, diags)
		t.Fatalf("expected ldap_plaintext_forbidden for ldap:// without start_tls")
	}
}

// TestKeeperAuthLDAP_StartTLSAllowed — ldap:// + start_tls: true допустим
// (StartTLS поднимает TLS поверх plaintext-порта).
func TestKeeperAuthLDAP_StartTLSAllowed(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldap://ldap.example.com:389"
    start_tls: true
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "ldap_plaintext_forbidden") {
		dump(t, diags)
		t.Fatalf("ldap:// + start_tls must NOT raise ldap_plaintext_forbidden")
	}
}

// TestKeeperAuthLDAP_TLSConflict — ldaps:// и start_tls взаимоисключимы.
func TestKeeperAuthLDAP_TLSConflict(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
    start_tls: true
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "ldap_tls_conflict") {
		dump(t, diags)
		t.Fatalf("expected ldap_tls_conflict for ldaps:// + start_tls")
	}
}

// TestKeeperAuthLDAP_SearchRequiresBindCreds — bind_mode=search (или пустой
// дефолт) без bind_dn/bind_password_ref → ошибки.
func TestKeeperAuthLDAP_SearchRequiresBindCreds(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "ldap_search_requires_bind_dn") {
		dump(t, diags)
		t.Fatalf("expected ldap_search_requires_bind_dn (search default without bind_dn)")
	}
	if !hasCode(diags, "ldap_search_requires_bind_password") {
		dump(t, diags)
		t.Fatalf("expected ldap_search_requires_bind_password (search default without bind_password_ref)")
	}
}

// TestKeeperAuthLDAP_UnsupportedBindMode — bind_mode вне {"", "search"} → ERROR
// на load (раньше ловилось только runtime в ldap.New): стадия 1 поддерживает
// только search-bind, direct отложен (code-nit, point 5).
func TestKeeperAuthLDAP_UnsupportedBindMode(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
    bind_mode: direct
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
    base_dn: "ou=people,dc=example,dc=com"
    user_filter: "(uid=%s)"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "ldap_bind_mode_unsupported") {
		dump(t, diags)
		t.Fatalf("expected ldap_bind_mode_unsupported for bind_mode=direct")
	}
}

// TestKeeperAuthLDAP_BindPasswordRefFormat — bind_password_ref не vault-ref →
// ошибка формата (тот же checkVaultRef, что у redis.password_ref).
func TestKeeperAuthLDAP_BindPasswordRefFormat(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: "plain-secret-not-a-ref"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected error for non-vault-ref bind_password_ref")
	}
}

// TestKeeperAuthLDAP_InsecureSkipVerifyWarns — insecure_skip_verify: true даёт
// WARN, но не блокирует загрузку (dev-only opt-out).
func TestKeeperAuthLDAP_InsecureSkipVerifyWarns(t *testing.T) {
	src := keeperBaseRequired + `auth:
  ldap:
    url: "ldaps://ldap.example.com:636"
    bind_dn: "cn=svc,dc=example,dc=com"
    bind_password_ref: vault:secret/keeper/ldap-bind
    tls:
      insecure_skip_verify: true
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "ldap_insecure_skip_verify") {
		dump(t, diags)
		t.Fatalf("expected ldap_insecure_skip_verify WARN")
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("insecure_skip_verify must WARN, not error")
	}
}
