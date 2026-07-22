package pluginhost

import (
	"bytes"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeCache is a minimal sigilCache for the adapter's unit test.
type fakeCache map[string]*keeperv1.PluginSigil

func (c fakeCache) Get(ns, name string) *keeperv1.PluginSigil { return c[ns+"."+name] }

// TestSigilLookupAdapter_Maps — keeperv1.PluginSigil is projected into
// shared.SigilRecord byte/field-exact: ManifestRaw→Manifest, signature, hex, ref.
func TestSigilLookupAdapter_Maps(t *testing.T) {
	sig := &keeperv1.PluginSigil{
		Namespace:    "core",
		Name:         "git",
		Ref:          "v2.0.0",
		BinarySha256: "abc123",
		Signature:    []byte{1, 2, 3, 4},
		Manifest:     []byte("kind: soul_module\nnamespace: core\nname: git\n"),
	}
	a := NewSigilLookupAdapter(fakeCache{"core.git": sig})

	rec := a.Get("core", "git")
	if rec == nil {
		t.Fatal("Get returned nil for present sigil")
	}
	if rec.Namespace != "core" || rec.Name != "git" || rec.Ref != "v2.0.0" {
		t.Errorf("identity mismatch: %+v", rec)
	}
	if rec.BinarySHA256hex != "abc123" {
		t.Errorf("BinarySHA256hex = %q", rec.BinarySHA256hex)
	}
	if !bytes.Equal(rec.Signature, []byte{1, 2, 3, 4}) {
		t.Errorf("Signature = %v", rec.Signature)
	}
	if !bytes.Equal(rec.Manifest, sig.GetManifest()) {
		t.Errorf("Manifest not byte-exact: %q vs %q", rec.Manifest, sig.GetManifest())
	}
}

// TestSigilLookupAdapter_AbsentIsNil — a missing grant → nil
// (verify treats it as no_sigil, fail-closed).
func TestSigilLookupAdapter_AbsentIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeCache{})
	if rec := a.Get("core", "missing"); rec != nil {
		t.Fatalf("absent sigil must map to nil, got %+v", rec)
	}
}

// TestSigilLookupAdapter_NilCache — a nil cache doesn't panic, always returns nil.
func TestSigilLookupAdapter_NilCache(t *testing.T) {
	a := NewSigilLookupAdapter(nil)
	if rec := a.Get("core", "git"); rec != nil {
		t.Fatalf("nil cache must yield nil record, got %+v", rec)
	}
}
