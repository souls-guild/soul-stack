package grpc

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// TestSigilRecordsToProto_MapsBothIdentitiesAndSchema — converting the set to the wire
// format carries the byte-exact signed schema (the verify canon, M1) and BOTH
// identities: the alias a Soul looks the grant up by, and the source the signature
// actually covers.
func TestSigilRecordsToProto_MapsBothIdentitiesAndSchema(t *testing.T) {
	recs := []*sigil.Sigil{{
		Alias:  "pkg",
		Source: "https://example.com/pkg.git",
		Ref:    "v1",
		Kind:   sharedplugin.SourceKindArtifact,
		// A two-platform release: the projection must carry BOTH rows, because the
		// signature is over the whole list and the Keeper does not get to decide
		// which platform the Soul on the far end is.
		Artifacts: []sharedhost.SigilArtifact{
			{OS: "linux", Arch: "amd64", Path: "pkg_linux_amd64", SHA256: "aa"},
			{OS: "linux", Arch: "arm64", Path: "pkg_linux_arm64", SHA256: "bb"},
		},
		Signature: []byte("sig"),
		Schema:    []byte(`{"kind":"soul_module","protocol_version":1}`),
	}}
	got := SigilRecordsToProto(recs)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	p := got[0]
	if p.GetAlias() != "pkg" || p.GetSource() != recs[0].Source || p.GetRef() != "v1" ||
		p.GetKind() != sharedplugin.SourceKindArtifact {
		t.Errorf("identity = %+v", p)
	}
	if len(p.GetArtifacts()) != 2 {
		t.Fatalf("artifacts = %d, want both rows of the release", len(p.GetArtifacts()))
	}
	for i, want := range recs[0].Artifacts {
		got := p.GetArtifacts()[i]
		if got.GetOs() != want.OS || got.GetArch() != want.Arch ||
			got.GetPath() != want.Path || got.GetSha256() != want.SHA256 {
			t.Errorf("artifacts[%d] = %+v, want %+v", i, got, want)
		}
	}
	if !bytes.Equal(p.GetSchema(), recs[0].Schema) {
		t.Errorf("schema = %q, want byte-equal %q", p.GetSchema(), recs[0].Schema)
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
		{Alias: "pkg", Source: "https://example.com/pkg.git", Ref: "v1",
			Kind: "git", Artifacts: []*keeperv1.SigilArtifact{{Sha256: "aa"}}},
		{Alias: "hetzner", Source: "https://example.com/hetzner.git", Ref: "v2",
			Kind: "git", Artifacts: []*keeperv1.SigilArtifact{{Sha256: "bb"}}},
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
		[]*keeperv1.PluginSigil{{Alias: "pkg", Source: "https://example.com/pkg.git"}}); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}
