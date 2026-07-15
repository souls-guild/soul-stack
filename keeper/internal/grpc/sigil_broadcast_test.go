package grpc

import (
	"bytes"
	"context"
	"errors"
	"testing"

	grpclib "google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
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

// TestBroadcastSigils_SendsSnapshotWithManifestRaw — the connect-time
// broadcast sends ONE SigilSnapshot (ReplaceAll, ADR-026(h)), not
// individual PluginSigil messages. Manifest inside the snapshot is
// byte-exact ManifestRaw (M1), not the JSONB projection.
func TestBroadcastSigils_SendsSnapshotWithManifestRaw(t *testing.T) {
	rec := &sigil.Sigil{
		Namespace:   "core",
		Name:        "template",
		Ref:         "v1.0.0",
		SHA256:      "deadbeef",
		Signature:   []byte("ed25519-sig"),
		ManifestRaw: []byte("raw: signed\nbytes: yes\n"),
		Manifest:    []byte(`{"raw":"signed"}`), // JSONB projection — must NOT make it onto the wire
	}
	h := newBroadcastHandler(t, &fakeSigilStore{recs: []*sigil.Sigil{rec}})
	stream := &fakeBidiStream{}

	h.broadcastSigils(context.Background(), stream, "sid", "sess")

	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (один SigilSnapshot)", len(stream.sent))
	}
	snap := stream.sent[0].GetSigilSnapshot()
	if snap == nil {
		t.Fatalf("payload = %T, want SigilSnapshot", stream.sent[0].GetPayload())
	}
	if len(snap.GetSigils()) != 1 {
		t.Fatalf("snapshot sigils = %d, want 1", len(snap.GetSigils()))
	}
	got := snap.GetSigils()[0]
	if got.GetNamespace() != "core" || got.GetName() != "template" || got.GetRef() != "v1.0.0" {
		t.Errorf("identity = %+v", got)
	}
	if got.GetBinarySha256() != "deadbeef" {
		t.Errorf("binary_sha256 = %q, want deadbeef", got.GetBinarySha256())
	}
	// CRITICAL (M1): Manifest = ManifestRaw byte-exact, NOT the JSONB projection.
	if !bytes.Equal(got.GetManifest(), rec.ManifestRaw) {
		t.Errorf("manifest = %q, want byte-equal ManifestRaw %q", got.GetManifest(), rec.ManifestRaw)
	}
	if bytes.Equal(got.GetManifest(), rec.Manifest) {
		t.Errorf("manifest equals JSONB-проекцию — должно быть ManifestRaw")
	}
	if !bytes.Equal(got.GetSignature(), rec.Signature) {
		t.Errorf("signature = %q, want %q", got.GetSignature(), rec.Signature)
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
		t.Fatalf("sent = %d, want 1 (пустой snapshot всё равно отправляется)", len(stream.sent))
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
		{Namespace: "core", Name: "a", Ref: "v1", SHA256: "aa", Signature: []byte("s1"), ManifestRaw: []byte("m1")},
		{Namespace: "core", Name: "b", Ref: "v1", SHA256: "bb", Signature: []byte("s2"), ManifestRaw: []byte("m2")},
	}
	h := newBroadcastHandler(t, &fakeSigilStore{recs: recs})
	stream := &fakeBidiStream{failAt: 1}
	// The single Send (snapshot) fails → the method doesn't panic and
	// doesn't return an error to the caller (best-effort); the stream will
	// close via its own recv loop.
	h.broadcastSigils(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (одна попытка Send, упавшая)", len(stream.sent))
	}
}
