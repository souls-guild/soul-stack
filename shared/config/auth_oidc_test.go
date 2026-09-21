package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// oidcValidBlock — a valid auth.oidc block (https issuer + client_id +
// redirect_url + vault-ref secret) whose individual fields the tests change.
const oidcValidBlock = `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    client_secret_ref: vault:secret/keeper/oidc-client-secret
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
    scopes: ["openid", "email", "groups"]
    groups_claim: groups
    aid_claim: sub
    group_role_map:
      ops: ["cluster-admin"]
`

// TestKeeperAuthOIDC_Valid — a valid block loads without errors.
func TestKeeperAuthOIDC_Valid(t *testing.T) {
	src := keeperBaseRequired + oidcValidBlock
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid auth.oidc block; got %d diags", len(diags))
	}
}

// TestKeeperAuthOIDC_IssuerNotHTTPS — http:// issuer is forbidden (TLS-required,
// ADR-058(g)): discovery/JWKS/token-exchange to the IdP must not go in the clear.
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

// TestKeeperAuthOIDC_ClientIDRequired — a missing client_id → error.
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

// TestKeeperAuthOIDC_RedirectURLRequired — a missing redirect_url → error.
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

// TestKeeperAuthOIDC_ClientSecretRefFormat — client_secret_ref that is not a
// vault-ref → format error (the same checkVaultRef as for redis.password_ref).
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

// TestKeeperAuthOIDC_PublicClientNoSecret — client_secret_ref is optional
// (public-client): without it the block is valid.
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

// TestKeeperAuthOIDC_MutableAIDClaimWarns — aid_claim from a user-mutable claim
// (email / preferred_username) → WARN (identity-spoofing risk, MED fix), NOT ERROR
// (the operator may deliberately choose it). `sub` (the default) — no WARN.
func TestKeeperAuthOIDC_MutableAIDClaimWarns(t *testing.T) {
	for _, claim := range []string{"email", "preferred_username"} {
		src := keeperBaseRequired + `auth:
  oidc:
    issuer: "https://idp.example.com/realms/soul"
    client_id: soul-keeper
    redirect_url: "https://keeper.example.com/auth/oidc/callback"
    aid_claim: ` + claim + `
`
		_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
		if diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("aid_claim=%s must WARN, not error", claim)
		}
		if !hasCode(diags, "oidc_aid_claim_mutable") {
			dump(t, diags)
			t.Fatalf("expected oidc_aid_claim_mutable WARN for aid_claim=%s", claim)
		}
	}
}
