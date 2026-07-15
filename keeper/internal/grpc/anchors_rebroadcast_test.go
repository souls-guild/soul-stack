package grpc

import (
	"context"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestOutbound_RebroadcastTrustAnchors_AllLocalStreams — the PEM anchor set goes
// out to every locally connected Soul as ONE SigilTrustAnchors (ReplaceAll, R3-S6).
func TestOutbound_RebroadcastTrustAnchors_AllLocalStreams(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	outA := m.Register("sid-a")
	outB := m.Register("sid-b")
	ob := newOutboundForTest(t, m, nopAudit{})

	set := []string{
		"-----BEGIN PUBLIC KEY-----\nAAA\n-----END PUBLIC KEY-----\n",
		"-----BEGIN PUBLIC KEY-----\nBBB\n-----END PUBLIC KEY-----\n",
	}

	delivered := ob.RebroadcastTrustAnchors(context.Background(), set)
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (оба Soul-а)", delivered)
	}

	for name, out := range map[string]<-chan *keeperv1.FromKeeper{"sid-a": outA, "sid-b": outB} {
		select {
		case msg := <-out:
			ta := msg.GetSigilTrustAnchors()
			if ta == nil {
				t.Fatalf("%s: payload = %T, want SigilTrustAnchors", name, msg.GetPayload())
			}
			if len(ta.GetPubkeyPem()) != len(set) {
				t.Fatalf("%s: pubkey_pem = %d, want %d", name, len(ta.GetPubkeyPem()), len(set))
			}
		default:
			t.Fatalf("%s: ожидался один SigilTrustAnchors, канал пуст", name)
		}
		select {
		case extra := <-out:
			t.Fatalf("%s: лишнее сообщение после набора: %T", name, extra.GetPayload())
		default:
		}
	}
}

// TestOutbound_RebroadcastTrustAnchors_EmptySet — an empty set is sent as an empty
// SigilTrustAnchors (Soul clears its holder, near-instant retire).
func TestOutbound_RebroadcastTrustAnchors_EmptySet(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid-a")
	ob := newOutboundForTest(t, m, nopAudit{})

	delivered := ob.RebroadcastTrustAnchors(context.Background(), nil)
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	select {
	case msg := <-out:
		ta := msg.GetSigilTrustAnchors()
		if ta == nil {
			t.Fatalf("payload = %T, want SigilTrustAnchors", msg.GetPayload())
		}
		if len(ta.GetPubkeyPem()) != 0 {
			t.Fatalf("empty re-broadcast pubkey_pem = %d, want 0", len(ta.GetPubkeyPem()))
		}
	default:
		t.Fatal("пустой набор должен слать пустой SigilTrustAnchors, канал пуст")
	}
}

// TestOutbound_RebroadcastTrustAnchors_NoStreams — with no Souls connected,
// the broadcast is safe (nothing to send to).
func TestOutbound_RebroadcastTrustAnchors_NoStreams(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	ob := newOutboundForTest(t, m, nopAudit{})
	if got := ob.RebroadcastTrustAnchors(context.Background(), []string{"pem"}); got != 0 {
		t.Fatalf("delivered = %d, want 0", got)
	}
}

// TestOutbound_SendSigilTrustAnchors_DeliversToLocalStream — a single send of
// the set to a local stream.
func TestOutbound_SendSigilTrustAnchors_DeliversToLocalStream(t *testing.T) {
	m := NewStreamManager(discardLogger(t))
	out := m.Register("sid-a")
	ob := newOutboundForTest(t, m, nopAudit{})

	if err := ob.SendSigilTrustAnchors(context.Background(), "sid-a", []string{"pem-1"}); err != nil {
		t.Fatalf("SendSigilTrustAnchors: %v", err)
	}
	select {
	case msg := <-out:
		ta := msg.GetSigilTrustAnchors()
		if ta == nil || len(ta.GetPubkeyPem()) != 1 || ta.GetPubkeyPem()[0] != "pem-1" {
			t.Fatalf("неверный SigilTrustAnchors: %+v", ta)
		}
	default:
		t.Fatal("ожидался SigilTrustAnchors в канале")
	}
}
