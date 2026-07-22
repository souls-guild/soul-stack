package grpc

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestSigilRecordsToProto_MapsManifestRaw — converting the set to the wire
// format takes byte-exact ManifestRaw (the verify canon, M1), NOT the
// JSONB Manifest projection.
func TestSigilRecordsToProto_MapsManifestRaw(t *testing.T) {
	recs := []*sigil.Sigil{{
		Namespace:   "core",
		Name:        "pkg",
		Ref:         "v1",
		SHA256:      "aa",
		Signature:   []byte("sig"),
		ManifestRaw: []byte("raw: signed\nbytes: yes\n"),
		Manifest:    []byte(`{"raw":"signed"}`),
	}}
	got := SigilRecordsToProto(recs)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	if p.GetNamespace() != "core" || p.GetName() != "pkg" || p.GetRef() != "v1" ||
		p.GetBinarySha256() != "aa" {
		t.Errorf("identity = %+v", p)
	}
	if !bytes.Equal(p.GetManifest(), recs[0].ManifestRaw) {
		t.Errorf("manifest = %q, want byte-equal ManifestRaw %q", p.GetManifest(), recs[0].ManifestRaw)
	}
	if bytes.Equal(p.GetManifest(), recs[0].Manifest) {
		t.Errorf("manifest equals JSONB projection - should be ManifestRaw")
	}
}

func TestStreamManager_SIDs(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	if got := m.SIDs(); len(got) != 0 {
		t.Fatalf("empty manager: SIDs = %v, want []", got)
	}

	chA := m.Register("sid-a")
	chB := m.Register("sid-b")

	got := m.SIDs()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "sid-a" || got[1] != "sid-b" {
		t.Fatalf("SIDs = %v, want [sid-a sid-b]", got)
	}

	m.Unregister("sid-a", chA)
	got = m.SIDs()
	if len(got) != 1 || got[0] != "sid-b" {
		t.Fatalf("after Unregister SIDs = %v, want [sid-b]", got)
	}
	_ = chB
}

// TestOutbound_RebroadcastSigils_AllLocalStreams — the full active set goes
// out to every locally connected Soul as ONE SigilSnapshot (ReplaceAll, S6c).
func TestOutbound_RebroadcastSigils_AllLocalStreams(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	outA := m.Register("sid-a")
	outB := m.Register("sid-b")
	ob := newOutboundForTest(t, m, nopAudit{})

	set := []*keeperv1.PluginSigil{
		{Namespace: "core", Name: "pkg", Ref: "v1", BinarySha256: "aa"},
		{Namespace: "cloud", Name: "hetzner", Ref: "v2", BinarySha256: "bb"},
	}

	delivered := ob.RebroadcastSigils(context.Background(), set)
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (both Souls)", delivered)
	}

	for name, out := range map[string]<-chan *keeperv1.FromKeeper{"sid-a": outA, "sid-b": outB} {
		select {
		case msg := <-out:
			snap := msg.GetSigilSnapshot()
			if snap == nil {
				t.Fatalf("%s: payload = %T, want SigilSnapshot", name, msg.GetPayload())
			}
			if len(snap.GetSigils()) != len(set) {
				t.Fatalf("%s: snapshot sigils = %d, want %d", name, len(snap.GetSigils()), len(set))
			}
		default:
			t.Fatalf("%s: expected a single SigilSnapshot, channel empty", name)
		}
		// There should be nothing else in the channel — exactly one snapshot per stream.
		select {
		case extra := <-out:
			t.Fatalf("%s: extra message after snapshot: %T", name, extra.GetPayload())
		default:
		}
	}
}

// TestOutbound_RebroadcastSigils_EmptySet — an empty set is sent as an
// empty SigilSnapshot (ReplaceAll clears the cache on the Soul, near-instant
// revoke); every local Soul counts as delivered.
func TestOutbound_RebroadcastSigils_EmptySet(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid-a")
	ob := newOutboundForTest(t, m, nopAudit{})

	delivered := ob.RebroadcastSigils(context.Background(), nil)
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	select {
	case msg := <-out:
		snap := msg.GetSigilSnapshot()
		if snap == nil {
			t.Fatalf("payload = %T, want SigilSnapshot", msg.GetPayload())
		}
		if len(snap.GetSigils()) != 0 {
			t.Fatalf("empty re-broadcast snapshot sigils = %d, want 0", len(snap.GetSigils()))
		}
	default:
		t.Fatal("an empty set should send an empty snapshot (ReplaceAll erasure), channel empty")
	}
}

// TestOutbound_RebroadcastSigils_NoStreams — with no connected Souls,
// distribution is safe (nobody to send to).
func TestOutbound_RebroadcastSigils_NoStreams(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, m, nopAudit{})
	if got := ob.RebroadcastSigils(context.Background(),
		[]*keeperv1.PluginSigil{{Namespace: "core", Name: "pkg"}}); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}
