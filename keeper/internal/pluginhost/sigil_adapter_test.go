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

// TestSigilLookupAdapter_Maps verifies Get resolves record by (namespace, name) pair.
func TestSigilLookupAdapter_Maps(t *testing.T) {
	want := &sharedhost.SigilRecord{
		Namespace:       "soulstack",
		Name:            "hetzner",
		Ref:             "v2.0.0",
		BinarySHA256hex: "abc123",
		Signature:       []byte{1, 2, 3, 4},
		Manifest:        []byte("kind: cloud_driver\nnamespace: soulstack\nname: hetzner\n"),
	}
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{want}}, nil)

	rec := a.Get("soulstack", "hetzner")
	if rec != want {
		t.Fatalf("Get returned %+v, want %+v", rec, want)
	}
}

// TestSigilLookupAdapter_AbsentIsNil verifies no record for pair → nil (no_sigil).
func TestSigilLookupAdapter_AbsentIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{
		{Namespace: "soulstack", Name: "other"},
	}}, nil)
	if rec := a.Get("soulstack", "hetzner"); rec != nil {
		t.Fatalf("absent sigil must map to nil, got %+v", rec)
	}
}

// TestSigilLookupAdapter_SingleSlotNewestWins verifies multiple records per pair
// → first match (ListActive returns newest first: allowed_at DESC).
func TestSigilLookupAdapter_SingleSlotNewestWins(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{recs: []*sharedhost.SigilRecord{
		{Namespace: "soulstack", Name: "hetzner", Ref: "v2"},
		{Namespace: "soulstack", Name: "hetzner", Ref: "v1"},
	}}, nil)
	rec := a.Get("soulstack", "hetzner")
	if rec == nil || rec.Ref != "v2" {
		t.Fatalf("expected newest (ref=v2), got %+v", rec)
	}
}

// TestSigilLookupAdapter_NilLister verifies nil-lister doesn't panic, always nil
// (no_sigil fail-closed on incomplete wire-up / Sigil off).
func TestSigilLookupAdapter_NilLister(t *testing.T) {
	a := NewSigilLookupAdapter(nil, nil)
	if rec := a.Get("soulstack", "hetzner"); rec != nil {
		t.Fatalf("nil lister must yield nil record, got %+v", rec)
	}
}

// TestSigilLookupAdapter_ListErrorIsNil verifies registry read error → nil
// (verify fail-closed: error ≠ "allow").
func TestSigilLookupAdapter_ListErrorIsNil(t *testing.T) {
	a := NewSigilLookupAdapter(fakeLister{err: errors.New("db down")}, nil)
	if rec := a.Get("soulstack", "hetzner"); rec != nil {
		t.Fatalf("list error must yield nil record, got %+v", rec)
	}
}
