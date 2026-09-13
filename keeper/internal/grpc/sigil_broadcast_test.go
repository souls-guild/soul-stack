package grpc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	grpclib "google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// fakeSigilStore — a configurable [SigilStore] implementation for
// broadcast tests.
type fakeSigilStore struct {
	recs []*sigil.Sigil
	err  error
}

func (f *fakeSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) {
	return f.recs, f.err
}

// fakeBidiStream — a minimal [grpclib.BidiStreamingServer] for calling
// broadcastSigils directly: captures the sent FromKeeper messages and can
// simulate a Send failure. ServerStream is embedded as nil — broadcastSigils
// only calls Send (Context comes from the passed ctx, not from the stream).
type fakeBidiStream struct {
	grpclib.ServerStream
	sent    []*keeperv1.FromKeeper
	failAt  int // 1-based index of the Send call to fail on (0 = never fail)
	sendErr error
}

func (s *fakeBidiStream) Recv() (*keeperv1.FromSoul, error) {
	return nil, errors.New("fakeBidiStream: Recv not used in broadcast tests")
}

func (s *fakeBidiStream) Send(m *keeperv1.FromKeeper) error {
	s.sent = append(s.sent, m)
	if s.failAt > 0 && len(s.sent) == s.failAt {
		if s.sendErr != nil {
			return s.sendErr
		}
		return errors.New("forced send failure")
	}
	return nil
}

func newBroadcastHandler(t *testing.T, store SigilStore) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: nopAudit{},
		KID:         "kid-test",
		SigilStore:  store,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

// TestBroadcastSigils_SendsSnapshotWithSignedSchema — the connect-time broadcast sends
// ONE SigilSnapshot (ReplaceAll, ADR-026(h)), not individual PluginSigil messages. The
// schema inside it is the byte-exact bytes the signature covers, and both identities
// ride: the alias so a Soul can look the grant up, the source because that is what was
// signed.
func TestBroadcastSigils_SendsSnapshotWithSignedSchema(t *testing.T) {
	rec := &sigil.Sigil{
		Alias:     "template",
		Source:    "https://example.com/template.git",
		Ref:       "v1.0.0",
		Kind:      sharedplugin.SourceKindGit,
		Artifacts: []sharedhost.SigilArtifact{{SHA256: "deadbeef"}},
		Signature: []byte("ed25519-sig"),
		Schema:    []byte(`{"kind":"soul_module","protocol_version":1}`),
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
	}
	h := newBroadcastHandler(t, &fakeSigilStore{recs: []*sigil.Sigil{rec}})
	stream := &fakeBidiStream{}

	h.broadcastSigils(context.Background(), stream, "sid", "sess")

	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (a single SigilSnapshot)", len(stream.sent))
	}
	snap := stream.sent[0].GetSigilSnapshot()
	if snap == nil {
		t.Fatalf("payload = %T, want SigilSnapshot", stream.sent[0].GetPayload())
	}
	if len(snap.GetSigils()) != 1 {
		t.Fatalf("snapshot sigils = %d, want 1", len(snap.GetSigils()))
	}
	got := snap.GetSigils()[0]
	if got.GetAlias() != "template" || got.GetSource() != rec.Source || got.GetRef() != "v1.0.0" {
		t.Errorf("identity = %+v", got)
	}
	if got.GetKind() != sharedplugin.SourceKindGit {
		t.Errorf("kind = %q, want %q", got.GetKind(), sharedplugin.SourceKindGit)
	}
	if len(got.GetArtifacts()) != 1 || got.GetArtifacts()[0].GetSha256() != "deadbeef" {
		t.Errorf("artifacts = %+v, want the one row deadbeef", got.GetArtifacts())
	}
	// CRITICAL (M1): the schema on the wire is the byte-exact signed document — a Soul
	// re-hashes exactly these bytes, so anything re-derived here would verify against
	// nothing.
	if !bytes.Equal(got.GetSchema(), rec.Schema) {
		t.Errorf("schema = %q, want byte-equal %q", got.GetSchema(), rec.Schema)
	}
	if !bytes.Equal(got.GetSignature(), rec.Signature) {
		t.Errorf("signature = %q, want %q", got.GetSignature(), rec.Signature)
	}
	// commit_sha is Keeper-side audit and sits OUTSIDE the signed block, so it must not
	// ride: on the wire it would be an unverifiable claim.
	if strings.Contains(got.String(), rec.CommitSHA) {
		t.Errorf("commit_sha leaked onto the wire: %v", got)
	}
}

func TestBroadcastSigils_NilStoreNoOp(t *testing.T) {
	h := newBroadcastHandler(t, nil)
	stream := &fakeBidiStream{}
	h.broadcastSigils(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 (Sigil off → no-op)", len(stream.sent))
	}
}

// TestBroadcastSigils_EmptyListSendsEmptySnapshot — with an empty registry
// (but Sigil enabled), connect-time still sends an empty snapshot: on
// reconnect the Soul will use ReplaceAll to bring its cache to "no plugin
// is granted" (S6c).
func TestBroadcastSigils_EmptyListSendsEmptySnapshot(t *testing.T) {
	h := newBroadcastHandler(t, &fakeSigilStore{recs: nil})
	stream := &fakeBidiStream{}
	h.broadcastSigils(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (an empty snapshot is still sent)", len(stream.sent))
	}
	snap := stream.sent[0].GetSigilSnapshot()
	if snap == nil {
		t.Fatalf("payload = %T, want SigilSnapshot", stream.sent[0].GetPayload())
	}
	if len(snap.GetSigils()) != 0 {
		t.Fatalf("empty registry snapshot sigils = %d, want 0", len(snap.GetSigils()))
	}
}

func TestBroadcastSigils_ListErrorDoesNotPanicAndSkips(t *testing.T) {
	h := newBroadcastHandler(t, &fakeSigilStore{err: errors.New("pg down")})
	stream := &fakeBidiStream{}
	// Should not panic and should not send anything — the stream stays
	// alive (broadcast is best-effort, fail-closed verify on the Soul
	// protects it).
	h.broadcastSigils(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 (ListActive error → skip)", len(stream.sent))
	}
}

func TestBroadcastSigils_SendFailDoesNotPanic(t *testing.T) {
	recs := []*sigil.Sigil{
		{Alias: "a", Source: "https://example.com/a.git", Ref: "v1",
			Artifacts: []sharedhost.SigilArtifact{{SHA256: "aa"}}, Signature: []byte("s1"), Schema: []byte("m1")},
		{Alias: "b", Source: "https://example.com/b.git", Ref: "v1",
			Artifacts: []sharedhost.SigilArtifact{{SHA256: "bb"}}, Signature: []byte("s2"), Schema: []byte("m2")},
	}
	h := newBroadcastHandler(t, &fakeSigilStore{recs: recs})
	stream := &fakeBidiStream{failAt: 1}
	// The single Send (snapshot) fails → the method doesn't panic and
	// doesn't return an error to the caller (best-effort); the stream will
	// close via its own recv loop.
	h.broadcastSigils(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (a single Send attempt, which failed)", len(stream.sent))
	}
}
