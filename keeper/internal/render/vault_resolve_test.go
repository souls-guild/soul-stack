package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestResolveVaultRefs_NoRefs — params without vault refs pass through with no
// Vault call (PM-decision 2, no-op). vc=nil is safe.
func TestResolveVaultRefs_NoRefs(t *testing.T) {
	params := map[string]any{
		"cmd":    "echo hello",
		"nested": map[string]any{"k": "v"},
		"list":   []any{"a", "b"},
		"num":    int64(7),
	}
	out, err := resolveVaultRefs(context.Background(), nil, params)
	if err != nil {
		t.Fatalf("resolveVaultRefs: %v", err)
	}
	if out["cmd"] != "echo hello" {
		t.Errorf("command = %v", out["cmd"])
	}
	if n := out["nested"].(map[string]any); n["k"] != "v" {
		t.Errorf("nested = %v", n)
	}
}

// TestResolveVaultRefs_Empty — empty/nil params → no-op.
func TestResolveVaultRefs_Empty(t *testing.T) {
	out, err := resolveVaultRefs(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("resolveVaultRefs(nil): %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

// TestReadVaultRef_InterpolationMarker — ${…} inside a vault ref → error (a
// vault ref must be a static string, phase boundary ADR-010).
func TestReadVaultRef_InterpolationMarker(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/${ input.x }")
	if err == nil {
		t.Fatal("expected an error for ${...} in vault-ref")
	}
	if !strings.Contains(err.Error(), "static string") {
		t.Errorf("err = %v", err)
	}
}

// TestReadVaultRef_NilClient — a vault ref with no Vault client → error.
func TestReadVaultRef_NilClient(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/password")
	if err == nil {
		t.Fatal("expected a nil-client error when a vault-ref is present")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v", err)
	}
}

// TestReadVaultRef_EmptyField — an empty field name after '#' → error.
func TestReadVaultRef_EmptyField(t *testing.T) {
	_, err := readVaultRef(context.Background(), nil, "vault:secret/db/creds#")
	if err == nil {
		t.Fatal("expected an error for an empty field after '#'")
	}
}

// TestResolveVaultRefs_RefDetectedInNested — a ref nested deep in the structure
// is detected (reaches readVaultRef, which fails without a client — confirms
// the traversal).
func TestResolveVaultRefs_RefDetectedInNested(t *testing.T) {
	params := map[string]any{
		"outer": map[string]any{
			"list": []any{"plain", "vault:secret/db/password"},
		},
	}
	_, err := resolveVaultRefs(context.Background(), nil, params)
	if err == nil {
		t.Fatal("expected an error: ref in a nested struct must reach Vault")
	}
}

// resolveVaultStubKV — a KVReader with a REALISTIC not-found (like
// keeper/internal/vault: `vault: KV path not found: <plain path>`, path WITHOUT
// the vault: prefix). Separate from pipelineStubKV, which artificially prefixes
// `vault:` to the path (simulating an old leak) and so isn't suitable for
// checking actionable text.
type resolveVaultStubKV struct{ secrets map[string]map[string]any }

func (s resolveVaultStubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	data, ok := s.secrets[path]
	if !ok {
		return nil, errors.New("vault: KV path not found: " + path)
	}
	return data, nil
}

// TestReadVaultRef_MissingSecretActionable (NIM-73): a not-found vault ref in
// params gives an actionable error — the path in FLAT form, survives production
// masking of status_details/error_summary (symmetric with shared/cel.callVault).
func TestReadVaultRef_MissingSecretActionable(t *testing.T) {
	kv := resolveVaultStubKV{secrets: map[string]map[string]any{}}
	_, err := readVaultRef(context.Background(), kv, "vault:secret/redis/nosql/users/alice#password")
	if err == nil {
		t.Fatal("expected a missing-secret error")
	}
	assertResolveVaultActionable(t, err.Error(), "secret/redis/nosql/users/alice")
}

// TestReadVaultRef_MissingFieldActionable (NIM-73): the secret exists, the
// field doesn't → actionable path+field, survives masking; other fields' values
// don't leak.
func TestReadVaultRef_MissingFieldActionable(t *testing.T) {
	kv := resolveVaultStubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "TOP-SECRET-VALUE"},
	}}
	_, err := readVaultRef(context.Background(), kv, "vault:secret/redis/admin#nope")
	if err == nil {
		t.Fatal("expected a missing-field error")
	}
	assertResolveVaultActionable(t, err.Error(), "secret/redis/admin")
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("text does not name the missing field nope: %q", err.Error())
	}
	if strings.Contains(err.Error(), "TOP-SECRET-VALUE") {
		t.Fatalf("another secret field's value leaked: %q", err.Error())
	}
}

// assertResolveVaultActionable: the vault-ref error text (a) carries the path in
// FLAT form, (b) does NOT carry the vault: ref marker, (c) survives production
// masking (audit.MaskSecretsSealed) — not `***MASKED***`, the path stays visible.
func assertResolveVaultActionable(t *testing.T, errText, path string) {
	t.Helper()
	if !strings.Contains(errText, path) {
		t.Fatalf("text does not carry path %q: %q", path, errText)
	}
	if strings.Contains(errText, "vault:"+path) {
		t.Fatalf("text carries the vault:-ref form (masking would eat it whole): %q", errText)
	}
	masked := audit.MaskSecretsSealed(map[string]any{"error": errText}, audit.SealOpts{})
	got, _ := masked["error"].(string)
	if got == "***MASKED***" {
		t.Fatalf("actionable error is masked entirely: %q", got)
	}
	if !strings.Contains(got, path) {
		t.Fatalf("path %q disappeared after masking: %q", path, got)
	}
}
