package pluginhost

import (
	"bytes"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeCache is a minimal sigilCache for the adapter's unit test.
type fakeCache map[string]*keeperv1.PluginSigil

func (c fakeCache) Get(alias string) *keeperv1.PluginSigil { return c[alias] }

// TestSigilLookupAdapter_Maps — keeperv1.PluginSigil is projected into
// shared.SigilRecord field-exact, with the schema bytes carried across untouched: the
// adapter is the one mapping point, and anything it drops or rewrites would make
// verify hash something other than what Keeper signed.
func TestSigilLookupAdapter_Maps(t *testing.T) {
	sig := &keeperv1.PluginSigil{
		Alias:  "redis",
		Source: "https://github.com/souls-guild/soul-mod-redis",
		Ref:    "v2.0.0",
		Kind:   "artifact",
		Artifacts: []*keeperv1.SigilArtifact{
			{Os: "linux", Arch: "amd64", Path: "redis_linux_amd64", Sha256: "abc123"},
			{Os: "linux", Arch: "arm64", Path: "redis_linux_arm64", Sha256: "def456"},
		},
		Signature: []byte{1, 2, 3, 4},
		Schema:    []byte(`{"kind":"soul_module","protocol_version":1}`),
	}
	a := NewSigilLookupAdapter(fakeCache{"redis": sig})

	rec := a.Get("redis")
	if rec == nil {
		t.Fatal("Get returned nil for present sigil")
	}
	if rec.Alias != "redis" || rec.Source != sig.GetSource() || rec.Ref != "v2.0.0" {
		t.Errorf("identity mismatch: %+v", rec)
	}
	if rec.Kind != "artifact" {
		t.Errorf("Kind = %q, want artifact", rec.Kind)
	}
	// The WHOLE list crosses, unfiltered: the signature is over every row, so an
	// adapter that dropped the other platforms' rows would leave a record that cannot
	// verify at all.
	if len(rec.Artifacts) != 2 {
		t.Fatalf("Artifacts = %d, want both rows of the release", len(rec.Artifacts))
	}
	for i, want := range sig.GetArtifacts() {
		got := rec.Artifacts[i]
		if got.OS != want.GetOs() || got.Arch != want.GetArch() ||
			got.Path != want.GetPath() || got.SHA256 != want.GetSha256() {
			t.Errorf("Artifacts[%d] = %+v, want %+v", i, got, want)
		}
	}
	if !bytes.Equal(rec.Signature, []byte{1, 2, 3, 4}) {
		t.Errorf("Signature = %v", rec.Signature)
	}
	if !bytes.Equal(rec.Schema, sig.GetSchema()) {
		t.Errorf("Schema not byte-exact: %q vs %q", rec.Schema, sig.GetSchema())
	}
}

// TestSigilLookupAdapter_KeyIsTheAlias — the lookup key is the registration alias and
// nothing else. A grant filed under a different alias is not this slot's grant, even
// when it points at the same source.
func TestSigilLookupAdapter_KeyIsTheAlias(t *testing.T) {
	sig := &keeperv1.PluginSigil{
		Alias:  "redis-community",
		Source: "https://github.com/souls-guild/soul-mod-redis",
	}
	a := NewSigilLookupAdapter(fakeCache{"redis-community": sig})

	if rec := a.Get("redis"); rec != nil {
		t.Fatalf("a grant under another alias resolved for `redis`: %+v", rec)
	}
	if rec := a.Get("redis-community"); rec == nil {
		t.Fatal("the grant did not resolve under its own alias")
	}
}

// TestSigilLookupAdapter_AbsentIsNil — a missing grant → nil
// (verify treats it as no_sigil, fail-closed).
func TestSigilLookupAdapter_AbsentIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeCache{})
	if rec := a.Get("missing"); rec != nil {
		t.Fatalf("absent sigil must map to nil, got %+v", rec)
	}
}

// TestSigilLookupAdapter_NilCache — a nil cache doesn't panic, always returns nil.
func TestSigilLookupAdapter_NilCache(t *testing.T) {
	a := NewSigilLookupAdapter(nil)
	if rec := a.Get("redis"); rec != nil {
		t.Fatalf("nil cache must yield nil record, got %+v", rec)
	}
}
