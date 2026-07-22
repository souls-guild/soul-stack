package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// revealBase — a valid service.yml + an injectable revealable_secrets section (NIM-74).
const revealBase = "name: redis\nstate_schema_version: 1\nstate_schema:\n  type: object\n"

// TestRevealableSecrets_Valid — a correct declaration: 0 errors, fields parsed.
func TestRevealableSecrets_Valid(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    label: "Redis user password"
    enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a valid revealable_secrets section produced errors")
	}
	if len(cfg.RevealableSecrets) != 1 {
		t.Fatalf("RevealableSecrets = %d, want 1", len(cfg.RevealableSecrets))
	}
	rs := cfg.RevealableSecrets[0]
	if rs.ID != "user_password" || rs.Enumerate != "state.redis_users" ||
		rs.VaultRef != "secret/{service}/{incarnation}/users/{key}#password" {
		t.Errorf("field parsing is incorrect: %#v", rs)
	}
}

// TestRevealableSecrets_VaultRefMissingKey — enumerate set, but vault_ref without {key} →
// vault_ref_missing_key (per-element reveal is inexpressible without {key}).
func TestRevealableSecrets_VaultRefMissingKey(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/fixed#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "vault_ref_missing_key") {
		dump(t, diags)
		t.Fatal("expected vault_ref_missing_key (enumerate set, {key} missing)")
	}
}

// TestRevealableSecrets_UnknownPlaceholder — a placeholder outside {incarnation}/{key} →
// vault_ref_unknown_placeholder.
func TestRevealableSecrets_UnknownPlaceholder(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}/{field}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "vault_ref_unknown_placeholder") {
		dump(t, diags)
		t.Fatal("expected vault_ref_unknown_placeholder for {field}")
	}
}

// TestRevealableSecrets_DuplicateID — two identical ids → duplicate_id.
func TestRevealableSecrets_DuplicateID(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
  - id: user_password
    enumerate: state.other_users
    vault_ref: "secret/{service}/{incarnation}/other/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duplicate_id") {
		dump(t, diags)
		t.Fatal("expected duplicate_id for the duplicate user_password")
	}
}

// TestRevealableSecrets_MissingEnumerate — enumerate is required (MVP) →
// missing_required_field.
func TestRevealableSecrets_MissingEnumerate(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatal("expected missing_required_field for the missing enumerate")
	}
}

// TestRevealableSecrets_BadEnumerateForm — enumerate outside the state.<segment> form →
// enumerate_invalid_format.
func TestRevealableSecrets_BadEnumerateForm(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "enumerate_invalid_format") {
		dump(t, diags)
		t.Fatal("expected enumerate_invalid_format (missing state. prefix)")
	}
}

// TestRevealableSecrets_VaultRefNotServiceScoped — ★ C1 defense-in-depth: a vault_ref
// without {service} (even with {incarnation}) → vault_ref_not_service_scoped
// (the path must live under this specific service's secret namespace).
func TestRevealableSecrets_VaultRefNotServiceScoped(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: state.redis_users
    vault_ref: "secret/redis/{incarnation}/users/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "vault_ref_not_service_scoped") {
		dump(t, diags)
		t.Fatal("expected vault_ref_not_service_scoped (missing {service})")
	}
}

// TestRevealableSecrets_VaultRefMissingField — a vault_ref without #<field> →
// vault_ref_missing_field (reveal exposes exactly one scalar field).
func TestRevealableSecrets_VaultRefMissingField(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - id: user_password
    enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "vault_ref_missing_field") {
		dump(t, diags)
		t.Fatal("expected vault_ref_missing_field (missing #<field>)")
	}
}

// TestRevealableSecrets_MissingID — id is required → missing_required_field.
func TestRevealableSecrets_MissingID(t *testing.T) {
	src := revealBase + `revealable_secrets:
  - enumerate: state.redis_users
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatal("expected missing_required_field for the missing id")
	}
}
