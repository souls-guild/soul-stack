package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// oidcValidBlock — корректный блок auth.oidc (https issuer + client_id +
// redirect_url + vault-ref secret), к которому тесты меняют отдельные поля.
const oidcValidBlock = `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    client_secret_ref: vault:secret/keeper/oidc-client-secret
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
    scopes: ["openid", "email", "groups"]
    groups_claim: groups
    aid_claim: preferred_username
    group_role_map:
      ops: ["cluster-admin"]
`

// TestKeeperAuthOIDC_Valid — корректный блок грузится без ошибок.
func TestKeeperAuthOIDC_Valid(t *testing.T) {
	src := keeperBaseRequired + oidcValidBlock
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid auth.oidc block; got %d diags", len(diags))
	}
}

// TestKeeperAuthOIDC_IssuerNotHTTPS — http:// issuer запрещён (TLS-required,
// ADR-058(g)): discovery/JWKS/token-exchange к IdP не должны идти открыто.
func TestKeeperAuthOIDC_IssuerNotHTTPS(t *testing.T) {
	src := keeperBaseRequired + `auth:
  oidc:
    issuer: "http://idp.example.com/realms/soul"
    client_id: soul-keeper
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "oidc_issuer_not_https") {
		dump(t, diags)
		t.Fatalf("expected oidc_issuer_not_https for http:// issuer")
	}
}

// TestKeeperAuthOIDC_ClientIDRequired — отсутствие client_id → ошибка.
func TestKeeperAuthOIDC_ClientIDRequired(t *testing.T) {
	src := keeperBaseRequired + `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "oidc_client_id_required") {
		dump(t, diags)
		t.Fatalf("expected oidc_client_id_required")
	}
}

// TestKeeperAuthOIDC_RedirectURLRequired — отсутствие redirect_url → ошибка.
func TestKeeperAuthOIDC_RedirectURLRequired(t *testing.T) {
	src := keeperBaseRequired + `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "oidc_redirect_url_required") {
		dump(t, diags)
		t.Fatalf("expected oidc_redirect_url_required")
	}
}

// TestKeeperAuthOIDC_ClientSecretRefFormat — client_secret_ref не vault-ref →
// ошибка формата (тот же checkVaultRef, что у redis.password_ref).
func TestKeeperAuthOIDC_ClientSecretRefFormat(t *testing.T) {
	src := keeperBaseRequired + `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
    client_secret_ref: "plain-secret-not-a-ref"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected error for non-vault-ref client_secret_ref")
	}
}

// TestKeeperAuthOIDC_PublicClientNoSecret — client_secret_ref опционален
// (public-client): без него блок валиден.
func TestKeeperAuthOIDC_PublicClientNoSecret(t *testing.T) {
	src := keeperBaseRequired + `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
`
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("public-client (no client_secret_ref) must be valid; got errors")
	}
}
