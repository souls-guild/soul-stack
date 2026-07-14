package config

import (
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestMatchesVaultScope — prefix-glob and exact match.
func TestMatchesVaultScope(t *testing.T) {
	cases := []struct {
		scope, logical string
		want           bool
	}{
		{"secret/services/redis/*", "secret/services/redis/prod#password", true},
		{"secret/services/redis/*", "secret/services/redis/", true},
		{"secret/services/redis/*", "secret/services/postgres/prod", false},
		{"secret/services/redis/*", "secret/keeper/jwt-signing-key", false},
		{"secret/services/redis/prod", "secret/services/redis/prod", true}, // exact
		{"secret/services/redis/prod", "secret/services/redis/prod2", false},
		{"", "secret/anything", false}, // empty scope matches nothing
	}
	for _, c := range cases {
		if got := MatchesVaultScope(c.scope, c.logical); got != c.want {
			t.Errorf("MatchesVaultScope(%q,%q)=%v want %v", c.scope, c.logical, got, c.want)
		}
	}
}

// TestDeniedByVaultFloor — the system floor is unconditional, extra adds to it.
//
// DeniedByVaultFloor is a pure prefix check over an ALREADY-normalized path:
// normalization (collapse `//`, reject `.`/`..`) is vault.ParseRef's job higher
// up the stack (Soul-safe shared/config with no vault client). This pins that:
// normalized equivalents of bypass paths are caught by the floor, while a raw
// un-normalized `secret//keeper/x` does NOT match on its own — so without
// normalization in ParseRef there would be a bypass (see parseref_test,
// security-regress).
func TestDeniedByVaultFloor(t *testing.T) {
	cases := []struct {
		logical string
		extra   []string
		want    bool
	}{
		{"secret/keeper/jwt-signing-key", nil, true},      // system-floor
		{"secret/internal/anything", nil, true},           // system-floor
		{"secret/services/redis/prod", nil, false},        // outside floor
		{"secret/team/x", []string{"secret/team/"}, true}, // config extension
		{"secret/team/x", []string{""}, false},            // empty extra ignored
		// normalized bypass equivalent → caught by the floor.
		{"secret/keeper/x", nil, true},
		// a raw un-normalized path is NOT caught by the floor (normalization is in ParseRef).
		{"secret//keeper/x", nil, false},
	}
	for _, c := range cases {
		if got := DeniedByVaultFloor(c.logical, c.extra); got != c.want {
			t.Errorf("DeniedByVaultFloor(%q,%v)=%v want %v", c.logical, c.extra, got, c.want)
		}
	}
}

// TestValidVaultScopeGlob — the prefix-glob form is validated.
func TestValidVaultScopeGlob(t *testing.T) {
	good := []string{"secret/services/redis/*", "secret/x", "kv/team/proj/*"}
	bad := []string{"", "*", "secret*", "noslash", "/leadingslash"}
	for _, s := range good {
		if !validVaultScopeGlob(s) {
			t.Errorf("validVaultScopeGlob(%q)=false, want true", s)
		}
	}
	for _, s := range bad {
		if validVaultScopeGlob(s) {
			t.Errorf("validVaultScopeGlob(%q)=true, want false", s)
		}
	}
}

// fakeResolver is an InputVaultResolver returning a fixed value for a ref in
// scope and emulating the rules (default-deny, scope/deny) via the same pure
// code as keeper-side. This checks the ResolveInputValuesVault phase model.
func fakeResolver(secret string, deny []string) InputVaultResolver {
	return func(name string, s *InputSchema, raw string) (any, error) {
		if s.VaultScope == "" {
			return nil, errors.New("default-deny: no vault_scope")
		}
		logical := raw[len("vault:"):]
		if i := indexByte(logical, '#'); i >= 0 {
			logical = logical[:i]
		}
		if !MatchesVaultScope(s.VaultScope, logical) {
			return nil, errors.New("out of scope")
		}
		if DeniedByVaultFloor(logical, deny) {
			return nil, errors.New("deny-list")
		}
		return secret, nil
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TestResolveInputValuesVault_RefInScope — a ref in scope resolves to a value.
func TestResolveInputValuesVault_RefInScope(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
  secret: true
  vault_scope: "secret/services/redis/*"
`)
	got, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_password": "vault:secret/services/redis/prod#password"},
		fakeResolver("s3cr3t-resolved-32ch", nil))
	if err != nil {
		t.Fatalf("ResolveInputValuesVault: %v", err)
	}
	if got["redis_password"] != "s3cr3t-resolved-32ch" {
		t.Fatalf("redis_password=%v, want resolved secret", got["redis_password"])
	}
}

// TestResolveInputValuesVault_NoScopeRejects — a ref in a field without
// vault_scope → error (default-deny, fork B), not treated as a literal.
func TestResolveInputValuesVault_NoScopeRejects(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
  secret: true
`)
	_, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_password": "vault:secret/services/redis/prod#password"},
		fakeResolver("x", nil))
	if err == nil {
		t.Fatal("ожидалась ошибка default-deny для поля без vault_scope")
	}
}

// TestResolveInputValuesVault_OutOfScopeRejects — a ref outside the scope prefix → error.
func TestResolveInputValuesVault_OutOfScopeRejects(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
  secret: true
  vault_scope: "secret/services/redis/*"
`)
	_, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_password": "vault:secret/services/postgres/prod#password"},
		fakeResolver("x", nil))
	if err == nil {
		t.Fatal("ожидалась ошибка out-of-scope")
	}
}

// TestResolveInputValuesVault_DenyListRejects — a ref in scope but on the
// deny-list → error (even if the scope mistakenly covers the floor).
func TestResolveInputValuesVault_DenyListRejects(t *testing.T) {
	schema := schemaFromInput(t, `bad_field:
  type: string
  required: true
  secret: true
  vault_scope: "secret/*"
`)
	_, err := ResolveInputValuesVault(schema,
		map[string]any{"bad_field": "vault:secret/keeper/jwt-signing-key#key"},
		fakeResolver("x", nil))
	if err == nil {
		t.Fatal("ожидалась ошибка deny-list для secret/keeper/*")
	}
}

// TestResolveInputValuesVault_LiteralPasses — a literal (non-vault:) value
// passes as before, the resolver is not called.
func TestResolveInputValuesVault_LiteralPasses(t *testing.T) {
	schema := schemaFromInput(t, `redis_password:
  type: string
  required: true
  secret: true
  vault_scope: "secret/services/redis/*"
  min_length: 16
`)
	got, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_password": "change-me-please-32"},
		func(name string, s *InputSchema, raw string) (any, error) {
			t.Fatalf("резолвер не должен вызываться для литерала, raw=%q", raw)
			return nil, nil
		})
	if err != nil {
		t.Fatalf("ResolveInputValuesVault: %v", err)
	}
	if got["redis_password"] != "change-me-please-32" {
		t.Fatalf("redis_password=%v", got["redis_password"])
	}
}

// TestResolveInputValuesVault_PatternOnResolved — the pattern is checked on the
// ALREADY-resolved value: the resolver returns a value failing the pattern →
// validation error (not a silent pass-through of the vault: string).
func TestResolveInputValuesVault_PatternOnResolved(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  required: true
  secret: true
  vault_scope: "secret/services/redis/*"
  pattern: "^[0-9]+\\.[0-9]+\\.[0-9]+$"
`)
	// the resolver returned garbage not matching the pattern.
	_, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_version": "vault:secret/services/redis/prod#version"},
		fakeResolver("not-a-version", nil))
	if err == nil {
		t.Fatal("ожидалась ошибка pattern на резолвнутом значении")
	}

	// the resolver returned a valid version → passes.
	got, err := ResolveInputValuesVault(schema,
		map[string]any{"redis_version": "vault:secret/services/redis/prod#version"},
		fakeResolver("7.2.4", nil))
	if err != nil {
		t.Fatalf("ResolveInputValuesVault (валидное): %v", err)
	}
	if got["redis_version"] != "7.2.4" {
		t.Fatalf("redis_version=%v", got["redis_version"])
	}
}

// scenarioDiags parses a scenario with an input: block and returns the
// diagnostics (for negative schema cases where schemaFromInput would Fatalf).
func scenarioDiags(t *testing.T, inputYAML string) []diag.Diagnostic {
	t.Helper()
	body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" + indentBlock(inputYAML, "  ")
	_, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	return diags
}

// TestVaultScope_RequiresSecret — vault_scope on a non-secret field → error.
func TestVaultScope_RequiresSecret(t *testing.T) {
	diags := scenarioDiags(t, `host:
  type: string
  vault_scope: "secret/x/*"
`)
	if !hasCode(diags, "input_vault_scope_requires_secret") {
		t.Fatalf("ожидался input_vault_scope_requires_secret; diags=%v", diags)
	}
}

// TestVaultScope_OnNonString — vault_scope on a non-string type → applicability error.
func TestVaultScope_OnNonString(t *testing.T) {
	diags := scenarioDiags(t, `n:
  type: integer
  secret: true
  vault_scope: "secret/x/*"
`)
	if !hasCode(diags, "input_key_invalid_for_type") {
		t.Fatalf("ожидался input_key_invalid_for_type; diags=%v", diags)
	}
}

// TestVaultScope_InvalidGlob — an invalid prefix-glob form → error.
func TestVaultScope_InvalidGlob(t *testing.T) {
	diags := scenarioDiags(t, `p:
  type: string
  secret: true
  vault_scope: "noslash"
`)
	if !hasCode(diags, "input_vault_scope_invalid") {
		t.Fatalf("ожидался input_vault_scope_invalid; diags=%v", diags)
	}
}

// TestVaultScope_ValidNoErrors — a correct declaration yields no errors.
func TestVaultScope_ValidNoErrors(t *testing.T) {
	diags := scenarioDiags(t, `redis_password:
  type: string
  required: true
  secret: true
  vault_scope: "secret/services/redis/*"
`)
	if diag.HasErrors(diags) {
		t.Fatalf("корректный vault_scope не должен давать ошибок; diags=%v", diags)
	}
}

// TestResolveInputValuesVault_NilResolver — a nil resolver leaves vault: refs
// untouched (back-compat: path without a vault client); the value passes as a string.
func TestResolveInputValuesVault_NilResolver(t *testing.T) {
	schema := schemaFromInput(t, `note:
  type: string
  secret: true
  vault_scope: "secret/x/*"
`)
	got, err := ResolveInputValuesVault(schema,
		map[string]any{"note": "vault:secret/x/y#f"}, nil)
	if err != nil {
		t.Fatalf("ResolveInputValuesVault(nil): %v", err)
	}
	if got["note"] != "vault:secret/x/y#f" {
		t.Fatalf("note=%v, ожидалась нетронутая строка", got["note"])
	}
}
