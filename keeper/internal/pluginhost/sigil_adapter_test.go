package pluginhost

import (
	"context"
	"errors"
	"testing"

	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// fakeLister is minimal SigilRecordLister for unit test of adapter.
type fakeLister struct {
	recs []*sharedhost.SigilRecord
	err  error
}

func (f fakeLister) ListActive(context.Context) ([]*sharedhost.SigilRecord, error) {
	return f.recs, f.err
}

// TestSigilLookupAdapter_Maps verifies Get resolves a record by REGISTRATION ALIAS —
// the key a host slot is named by and the only key the verify path holds.
func TestSigilLookupAdapter_Maps(t *testing.T) {
	want := &sharedhost.SigilRecord{
		Alias:           "hetzner",
		Source:          "https://example.com/soul-cloud-hetzner.git",
		Ref:             "v2.0.0",
		BinarySHA256hex: "abc123",
		Signature:       []byte{1, 2, 3, 4},
		Schema:          []byte(`{"kind":"cloud_driver","protocol_version":1}`),
	}
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{want}}, nil)

	rec := a.Get("hetzner")
	if rec != want {
		t.Fatalf("Get returned %+v, want %+v", rec, want)
	}
}

// TestSigilLookupAdapter_AbsentIsNil verifies no record for the alias → nil (no_sigil).
func TestSigilLookupAdapter_AbsentIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{
		{Alias: "other"},
	}}, nil)
	if rec := a.Get("hetzner"); rec != nil {
		t.Fatalf("absent sigil must map to nil, got %+v", rec)
	}
}

// TestSigilLookupAdapter_AliasIsNotSource pins that the lookup keys on the alias and
// NOT on what the grant was signed over: a host holding a slot named `hetzner` has no
// idea which source it came from, so a lookup by source could never be made.
func TestSigilLookupAdapter_AliasIsNotSource(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{
		{Alias: "hetzner", Source: "https://example.com/a.git", Ref: "v2"},
	}}, nil)
	if rec := a.Get("https://example.com/a.git"); rec != nil {
		t.Fatalf("Get must key on the alias, not the source: got %+v", rec)
	}
	if rec := a.Get("hetzner"); rec == nil || rec.Ref != "v2" {
		t.Fatalf("Get by alias failed: %+v", rec)
	}
}

// TestSigilLookupAdapter_NilLister verifies nil-lister doesn't panic, always nil
// (no_sigil fail-closed on incomplete wire-up / Sigil off).
func TestSigilLookupAdapter_NilLister(t *testing.T) {
	a := NewSigilLookupAdapter(nil, nil)
	if rec := a.Get("hetzner"); rec != nil {
		t.Fatalf("nil lister must yield nil record, got %+v", rec)
	}
}

// TestSigilLookupAdapter_ListErrorIsNil verifies registry read error → nil
// (verify fail-closed: error ≠ "allow").
func TestSigilLookupAdapter_ListErrorIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{err: errors.New("db down")}, nil)
	if rec := a.Get("hetzner"); rec != nil {
		t.Fatalf("list error must yield nil record, got %+v", rec)
	}
}

// TestSigilLookupAdapter_AliasIsALookupKeyNotATrustClaim is the keeper-side proof of
// the property the whole re-keying rests on, checked independently of the Soul side.
//
// The alias selects WHICH grant to check. It contributes nothing to whether that
// grant is valid — the signature covers (source, ref, binary_sha256, schema_sha256)
// and never the alias, which is why the same artifact under a second alias needs no
// second signature. Two things follow, and both are asserted here:
//
//   - the adapter answers only for the alias it was asked about, so a lookup can
//     never hand the verify path some other registration's grant. That would move an
//     approval between registrations while the digest and signature still checked
//     out — bytes approved for one alias running under another;
//   - the answer it returns is otherwise byte-identical between two registrations of
//     the same artifact: same source, same ref, same signature. Nothing about trust
//     changes with the name.
func TestSigilLookupAdapter_AliasIsALookupKeyNotATrustClaim(t *testing.T) {
	const source = "https://example.com/soul-mod-redis.git"
	sig := []byte{9, 9, 9}
	schema := []byte(`{"kind":"soul_module","protocol_version":1}`)
	// The same artifact registered twice. The rows differ ONLY in the alias — which
	// is exactly what the signed block does not cover.
	recs := []*sharedhost.SigilRecord{
		{Alias: "redis", Source: source, Ref: "v1", BinarySHA256hex: "aa", Signature: sig, Schema: schema},
		{Alias: "redis-community", Source: source, Ref: "v1", BinarySHA256hex: "aa", Signature: sig, Schema: schema},
	}
	a := NewSigilLookupAdapter(fakeLister{recs: recs}, nil)

	for _, alias := range []string{"redis", "redis-community"} {
		got := a.Get(alias)
		if got == nil {
			t.Fatalf("Get(%q) returned nil", alias)
		}
		// The selector held: the adapter never answers with a neighbouring grant.
		if got.Alias != alias {
			t.Errorf("Get(%q) answered with the grant for %q — a lookup that crosses registrations moves an approval", alias, got.Alias)
		}
		// The trust material is identical under either name.
		if got.Source != source || got.Ref != "v1" || string(got.Signature) != string(sig) {
			t.Errorf("Get(%q) trust material differs by alias: %+v", alias, got)
		}
	}

	// An alias nobody registered resolves to nothing rather than to "the closest
	// match" — fail-closed is the only safe answer to an unknown selector.
	if got := a.Get("redis-typo"); got != nil {
		t.Errorf("an unregistered alias resolved to %+v", got)
	}
}
