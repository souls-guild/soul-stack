package grpc

import (
	"context"
	"testing"
)

// fakeTrustAnchorSource — configurable [TrustAnchorSource] implementation for
// connect-time trust-anchor-set broadcast tests (R3-S6).
type fakeTrustAnchorSource struct {
	pems []string
}

func (f *fakeTrustAnchorSource) AnchorSetPEM() []string { return f.pems }

func newAnchorBroadcastHandler(t *testing.T, src TrustAnchorSource) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:       &fakeSeedDB{},
		AuditWriter:  nopAudit{},
		KID:          "kid-test",
		TrustAnchors: src,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

// TestBroadcastTrustAnchors_SendsSet — connect-time broadcast sends ONE
// SigilTrustAnchors with the full set of PEM anchors (R3-S6, ReplaceAll).
func TestBroadcastTrustAnchors_SendsSet(t *testing.T) {
	pems := []string{
		"-----BEGIN PUBLIC KEY-----\nAAA\n-----END PUBLIC KEY-----\n",
		"-----BEGIN PUBLIC KEY-----\nBBB\n-----END PUBLIC KEY-----\n",
	}
	h := newAnchorBroadcastHandler(t, &fakeTrustAnchorSource{pems: pems})
	stream := &fakeBidiStream{}

	h.broadcastTrustAnchors(context.Background(), stream, "sid", "sess")

	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (один SigilTrustAnchors)", len(stream.sent))
	}
	ta := stream.sent[0].GetSigilTrustAnchors()
	if ta == nil {
		t.Fatalf("payload = %T, want SigilTrustAnchors", stream.sent[0].GetPayload())
	}
	got := ta.GetPubkeyPem()
	if len(got) != 2 || got[0] != pems[0] || got[1] != pems[1] {
		t.Fatalf("pubkey_pem = %v, want %v", got, pems)
	}
}

// TestBroadcastTrustAnchors_NilSourceNoOp — TrustAnchors=nil (Sigil disabled) →
// no-op, nothing is sent.
func TestBroadcastTrustAnchors_NilSourceNoOp(t *testing.T) {
	h := newAnchorBroadcastHandler(t, nil)
	stream := &fakeBidiStream{}
	h.broadcastTrustAnchors(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 0 {
		t.Fatalf("sent = %d, want 0 (Sigil off → no-op)", len(stream.sent))
	}
}

// TestBroadcastTrustAnchors_EmptySetSendsEmpty — an empty set is still sent
// (empty = "no anchors" → Soul clears its holder, near-instant retire on reconnect).
func TestBroadcastTrustAnchors_EmptySetSendsEmpty(t *testing.T) {
	h := newAnchorBroadcastHandler(t, &fakeTrustAnchorSource{pems: nil})
	stream := &fakeBidiStream{}
	h.broadcastTrustAnchors(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (пустой набор всё равно отправляется)", len(stream.sent))
	}
	ta := stream.sent[0].GetSigilTrustAnchors()
	if ta == nil {
		t.Fatalf("payload = %T, want SigilTrustAnchors", stream.sent[0].GetPayload())
	}
	if len(ta.GetPubkeyPem()) != 0 {
		t.Fatalf("empty set pubkey_pem = %d, want 0", len(ta.GetPubkeyPem()))
	}
}

// TestBroadcastTrustAnchors_SendFailDoesNotPanic — a send failure does not panic
// (best-effort; the stream closes via its own recv-loop).
func TestBroadcastTrustAnchors_SendFailDoesNotPanic(t *testing.T) {
	h := newAnchorBroadcastHandler(t, &fakeTrustAnchorSource{pems: []string{"pem"}})
	stream := &fakeBidiStream{failAt: 1}
	h.broadcastTrustAnchors(context.Background(), stream, "sid", "sess")
	if len(stream.sent) != 1 {
		t.Fatalf("sent = %d, want 1 (одна попытка Send, упавшая)", len(stream.sent))
	}
}
