package render

import (
	"context"
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

// secretInputNames извлекает secret:true-имена из scenario-схемы.
func TestSecretInputNames(t *testing.T) {
	scn := &config.ScenarioManifest{Input: config.InputSchemaMap{
		"password": {Type: "string", Secret: true},
		"hostname": {Type: "string"},
		"api_key":  {Type: "string", Secret: true},
	}}
	got := secretInputNames(scn)
	if !got["password"] || !got["api_key"] {
		t.Errorf("secret-имена не собраны: %v", got)
	}
	if got["hostname"] {
		t.Errorf("несекретный hostname попал в набор: %v", got)
	}
}

// (a) secret-input значение в GENERIC-поле (content) → путь sealed.
// (f) обычное generic-поле (несекретный input) → НЕ sealed (нет over-seal).
func TestCollectSealed_SecretInputInGenericField(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"admin_password": true}}

	params := map[string]any{
		"content": "requirepass ${ input.admin_password }", // (a) generic-поле, секрет
		"port":    "${ input.port }",                       // (f) несекретный input
		"label":   "static-config",                         // чистый литерал
	}
	collectSealed(e, set, params, sources, "")

	paths := set.Paths()
	if !paths["content"] {
		t.Errorf("content (secret-input в generic-поле) не sealed: %v", paths)
	}
	if paths["port"] {
		t.Errorf("port (несекретный input) sealed — over-seal: %v", paths)
	}
	if paths["label"] {
		t.Errorf("label (литерал) sealed — over-seal: %v", paths)
	}
}

// (b) vault()-значение → путь sealed (без схемы — детектор ловит vault сам).
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
		t.Errorf("vault()-ячейка не sealed: %v", paths)
	}
	if paths["plain"] {
		t.Errorf("plain sealed — over-seal: %v", paths)
	}
}

// (c) тернарник, читающий secret-input → путь sealed (whole-cell).
func TestCollectSealed_TernaryReadsSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"tls_cert": true}}

	params := map[string]any{
		"cert": "${ has(input.tls_cert) ? input.tls_cert : '' }",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["cert"] {
		t.Errorf("тернарник с secret-input не sealed: %v", set.Paths())
	}
}

// (d) смешанное значение (literal + secret) → путь sealed (whole-value taint).
func TestCollectSealed_MixedLiteralSecret(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"password": true}}

	params := map[string]any{
		"line": "user=admin pass=${ input.password } host=db",
	}
	collectSealed(e, set, params, sources, "")
	if !set.Paths()["line"] {
		t.Errorf("смешанное literal+secret не sealed: %v", set.Paths())
	}
}

// Вложенность map/list — путь ведётся как renderValue (joinKey/joinIdx).
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
		t.Errorf("acl[0].secret не sealed: %v", paths)
	}
	if !paths["nested.token"] {
		t.Errorf("nested.token не sealed: %v", paths)
	}
}

// nil-Sealed → no-op (коллекция выключена, push/trial/Acolyte — БИТ-В-БИТ).
func TestCollectSealed_NilSetNoop(t *testing.T) {
	e := sealTestEngine(t)
	params := map[string]any{"x": "${ vault('secret/redis/admin#password') }"}
	// не паникует при nil-set
	collectSealed(e, nil, params, cel.SealSources{}, "")
	var nilSet *SealedSet
	if nilSet.Paths() != nil {
		t.Error("nil-SealedSet.Paths() должен быть nil")
	}
}

// Path-обход совпадает с renderValue (joinKey/joinIdx) — guard на расхождение,
// которое сломало бы соответствие sealed-путей путям маскинга.
func TestCollectSealed_PathConventionMatchesRenderValue(t *testing.T) {
	e := sealTestEngine(t)
	set := NewSealedSet()
	sources := cel.SealSources{SecretInputs: map[string]bool{"s": true}}
	params := map[string]any{"a": map[string]any{"b": []any{"${ input.s }"}}}
	collectSealed(e, set, params, sources, "")
	// renderValue для этого построил бы путь "a.b[0]".
	if !set.Paths()["a.b[0]"] {
		t.Errorf("путь не a.b[0]: %v", set.Paths())
	}
	_ = context.Background()
}
