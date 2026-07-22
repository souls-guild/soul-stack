package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

func sealTestEngine(t *testing.T) *cel.Engine {
	t.Helper()
	kv := stubKVRender{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t"},
	}}
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	return e
}

// secretInputNames extracts secret:true names from the scenario schema.
func TestSecretInputNames(t *testing.T) {
	scn := &config.ScenarioManifest{Input: config.InputSchemaMap{
		"password": {Type: "string", Secret: true},
		"hostname": {Type: "string"},
		"api_key":  {Type: "string", Secret: true},
	}}
	got := secretInputNames(scn)
	if !got["password"] || !got["api_key"] {
		t.Errorf("secret names not collected: %v", got)
	}
	if got["hostname"] {
		t.Errorf("non-secret hostname ended up in the set: %v", got)
	}
}

// (a) a secret-input value in a GENERIC field (content) → path sealed.
// (f) an ordinary generic field (non-secret input) → NOT sealed (no over-seal).
func TestCollectSealed_SecretInputInGenericField(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"admin_password": true}}

	params := map[string]any{
		"content": "requirepass ${ input.admin_password }", // (a) generic field, secret
		"port":    "${ input.port }",                       // (f) non-secret input
		"label":   "static-config",                         // pure literal
	}
	collectSealed(e, set, params, sources, "")

	paths := set.Paths()
	if !paths["content"] {
		t.Errorf("content (secret-input in a generic field) not sealed: %v", paths)
	}
	if paths["port"] {
		t.Errorf("port (non-secret input) sealed - over-seal: %v", paths)
	}
	if paths["label"] {
		t.Errorf("label (literal) sealed - over-seal: %v", paths)
	}
}

// (b) a vault() value → path sealed (no schema needed — the detector catches vault itself).
func TestCollectSealed_VaultValue(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	params := map[string]any{
		"token": "${ vault('secret/redis/admin#password') }",
		"plain": "no-secret",
	}
	collectSealed(e, set, params, cel.SealSources{}, "")

	paths := set.Paths()
	if !paths["token"] {
		t.Errorf("vault() cell not sealed: %v", paths)
	}
	if paths["plain"] {
		t.Errorf("plain sealed — over-seal: %v", paths)
	}
}

// (c) a ternary reading a secret-input → path sealed (whole-cell).
func TestCollectSealed_TernaryReadsSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"tls_cert": true}}

	params := map[string]any{
		"cert": "${ has(input.tls_cert) ? input.tls_cert : '' }",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["cert"] {
		t.Errorf("ternary with secret-input not sealed: %v", set.Paths())
	}
}

// (d) a mixed value (literal + secret) → path sealed (whole-value taint).
func TestCollectSealed_MixedLiteralSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"password": true}}

	params := map[string]any{
		"line": "user=admin pass=${ input.password } host=db",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["line"] {
		t.Errorf("mixed literal+secret not sealed: %v", set.Paths())
	}
}

// map/list nesting — the path is tracked the same way as renderValue (joinKey/joinIdx).
func TestCollectSealed_NestedPaths(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"pw": true}}

	params := map[string]any{
		"acl": []any{
			map[string]any{"name": "alice", "secret": "${ input.pw }"},
		},
		"nested": map[string]any{"token": "${ vault('secret/redis/admin#password') }"},
	}
	collectSealed(e, set, params, sources, "")

	paths := set.Paths()
	if !paths["acl[0].secret"] {
		t.Errorf("acl[0].secret not sealed: %v", paths)
	}
	if !paths["nested.token"] {
		t.Errorf("nested.token not sealed: %v", paths)
	}
}

// nil Sealed → no-op (collection disabled, push/trial/Acolyte — bit-for-bit).
func TestCollectSealed_NilSetNoop(t *testing.T) {
	e := sealTestEngine(t)
	params := map[string]any{"x": "${ vault('secret/redis/admin#password') }"}
	// doesn't panic on a nil set
	collectSealed(e, nil, params, cel.SealSources{}, "")
	var nilSet *SealedSet
	if nilSet.Paths() != nil {
		t.Error("nil-SealedSet.Paths() should be nil")
	}
}

// Path traversal matches renderValue (joinKey/joinIdx) — guards against a
// drift that would break the correspondence between sealed paths and masking paths.
func TestCollectSealed_PathConventionMatchesRenderValue(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"s": true}}
	params := map[string]any{"a": map[string]any{"b": []any{"${ input.s }"}}}
	collectSealed(e, set, params, sources, "")
	// renderValue would build the path "a.b[0]" for this.
	if !set.Paths()["a.b[0]"] {
		t.Errorf("path is not a.b[0]: %v", set.Paths())
	}
}

// The PEM content of a redis destiny task (core.file.present, content =
// vault(input.tls.<x>_ref)) gets marked sealed by the vault layer in the
// destiny pass. Guards PEM masking (ADR-010 §7.4): the destiny pass carries NO
// secret-input schema (scenarioSealSources returns an empty set), so the only
// thing that catches sealed for PEM is vault() IN THE content CELL ITSELF
// (collectSealed without a schema still detects vault). If someone replaces
// content with an already-resolved PEM via apply.input (`${ input.tls_cert }`
// without vault()) — this test fails: the destiny-input-secret schema isn't
// passed through, the cell stops being sealed, and the PEM would leak into
// error_summary/state. Mirrors L0 tls-enabled-standalone (same content shape there).
func TestCollectSealed_RedisTLSPEMContentViaVault(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()

	// Shape of the content cell for a redis destiny PEM task (server.yml).
	// sources without a schema — as in the destiny pass (destinyIn.Scenario
	// without Input → secretInputNames is empty).
	params := map[string]any{
		"path":    "/etc/redis/tls/redis.key",
		"content": "${ vault(input.tls.key_ref) }",
		"mode":    "0600",
		"owner":   "redis",
	}
	collectSealed(e, set, params, cel.SealSources{}, "")

	paths := set.Paths()
	if !paths["content"] {
		t.Errorf("PEM content (vault() in the cell) NOT sealed - PEM will leak into state/error: %v", paths)
	}
	if paths["path"] || paths["mode"] || paths["owner"] {
		t.Errorf("non-secret fields sealed - over-seal: %v", paths)
	}
}
