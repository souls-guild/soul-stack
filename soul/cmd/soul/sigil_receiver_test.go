package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/sigilcache"
)

// dispatchPayload replays the recv-loop handleSession branches for a single
// FromKeeper payload, the part relevant to Sigil (ADR-026(h)).
// Full handleSession needs a live *soulgrpc.StreamSession (mTLS stream) —
// that's covered by the integration test; here we isolate just the payload →
// cache / anchor-holder dispatch, to avoid spinning up gRPC just to check
// these cases.
//
// The authoritative allow-set path is SigilSnapshot → ReplaceAll. A single
// PluginSigil is a notification, it does NOT mutate the set. SigilTrustAnchors
// (R3-S6) → ReplaceAll on the anchor holder (fail-closed on a broken PEM). If
// the switch in main.go changes, keep this helper in sync — it deliberately
// duplicates the same logic.
func dispatchPayload(msg *keeperv1.FromKeeper, sigils *sigilcache.Cache, anchors *sharedhost.AnchorSet) {
	switch payload := msg.GetPayload().(type) {
	case *keeperv1.FromKeeper_SigilSnapshot:
		sigils.ReplaceAll(payload.SigilSnapshot.GetSigils())
	case *keeperv1.FromKeeper_SigilTrustAnchors:
		if anchors == nil {
			return
		}
		parsed, err := parseTrustAnchorSet(payload.SigilTrustAnchors.GetPubkeyPem())
		if err != nil {
			return // fail-closed: broken set — leave the holder untouched
		}
		anchors.SetAnchors(parsed)
	case *keeperv1.FromKeeper_PluginSigil:
		// no-op: a single notification doesn't change the set (Option A).
	default:
		// other payloads are out of scope for this test
	}
}

func snapshotMsg(sigs ...*keeperv1.PluginSigil) *keeperv1.FromKeeper {
	return &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_SigilSnapshot{
		SigilSnapshot: &keeperv1.SigilSnapshot{Sigils: sigs},
	}}
}

func pluginSigil(ns, name, ref, sha string) *keeperv1.PluginSigil {
	return &keeperv1.PluginSigil{Namespace: ns, Name: name, Ref: ref, BinarySha256: sha}
}

// TestReceiverAppliesSnapshot — SigilSnapshot populates the cache via ReplaceAll.
func TestReceiverAppliesSnapshot(t *testing.T) {
	sigils := sigilcache.New()

	dispatchPayload(snapshotMsg(
		pluginSigil("core", "pkg", "v1.2.3", "deadbeef"),
		pluginSigil("cloud", "hetzner", "v2", "cafef00d"),
	), sigils, nil)

	got := sigils.Get("core", "pkg")
	if got == nil {
		t.Fatal("receiver не положил snapshot-Sigil в кеш")
	}
	if got.GetRef() != "v1.2.3" || got.GetBinarySha256() != "deadbeef" {
		t.Fatalf("в кеше неверный Sigil: ref=%q sha=%q", got.GetRef(), got.GetBinarySha256())
	}
	if sigils.Get("cloud", "hetzner") == nil {
		t.Fatal("receiver потерял второй Sigil из snapshot-а")
	}
}

// TestReceiverSnapshotRevokesAbsent — near-instant revoke (S6c): a new snapshot
// without a previously allowed plugin erases it from the cache (ReplaceAll).
func TestReceiverSnapshotRevokesAbsent(t *testing.T) {
	sigils := sigilcache.New()

	// allow: snapshot with two allow entries.
	dispatchPayload(snapshotMsg(
		pluginSigil("core", "pkg", "v1", "aa"),
		pluginSigil("core", "file", "v1", "bb"),
	), sigils, nil)
	if sigils.Get("core", "pkg") == nil || sigils.Get("core", "file") == nil {
		t.Fatal("предусловие: оба допуска должны быть в кеше")
	}

	// revoke core/pkg: the new snapshot no longer includes it.
	dispatchPayload(snapshotMsg(pluginSigil("core", "file", "v1", "bb")), sigils, nil)

	if sigils.Get("core", "pkg") != nil {
		t.Fatal("near-instant revoke не сработал: отозванный допуск остался в кеше")
	}
	if sigils.Get("core", "file") == nil {
		t.Fatal("ReplaceAll по ошибке стёр действующий допуск")
	}
}

// TestReceiverEmptySnapshotClearsCache — an empty snapshot means no plugin is
// allowed (revoke all).
func TestReceiverEmptySnapshotClearsCache(t *testing.T) {
	sigils := sigilcache.New()
	dispatchPayload(snapshotMsg(pluginSigil("core", "pkg", "v1", "aa")), sigils, nil)
	if sigils.Get("core", "pkg") == nil {
		t.Fatal("предусловие: допуск должен быть в кеше")
	}

	dispatchPayload(snapshotMsg(), sigils, nil) // empty snapshot

	if sigils.Get("core", "pkg") != nil {
		t.Fatal("пустой snapshot должен очистить кеш (ни один плагин не допущен)")
	}
}

// TestReceiverSinglePluginSigilDoesNotMutate — a single PluginSigil (Option A)
// is a notification, it does NOT change the set: no additions, no replacements.
func TestReceiverSinglePluginSigilDoesNotMutate(t *testing.T) {
	sigils := sigilcache.New()

	// The authoritative set comes from the snapshot.
	dispatchPayload(snapshotMsg(pluginSigil("core", "pkg", "v1", "aa")), sigils, nil)

	// A single PluginSigil must NOT add a new one or replace an existing one.
	dispatchPayload(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_PluginSigil{
		PluginSigil: pluginSigil("core", "pkg", "v2", "bb"),
	}}, sigils, nil)
	dispatchPayload(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_PluginSigil{
		PluginSigil: pluginSigil("community", "new", "v1", "cc"),
	}}, sigils, nil)

	got := sigils.Get("core", "pkg")
	if got == nil || got.GetRef() != "v1" || got.GetBinarySha256() != "aa" {
		t.Fatalf("одиночный PluginSigil не должен менять авторитетный набор, получено %v", got)
	}
	if sigils.Get("community", "new") != nil {
		t.Fatal("одиночный PluginSigil не должен добавлять допуск в набор")
	}
}

// TestReceiverIgnoresNonSigilPayload — a non-Sigil payload doesn't touch the cache.
func TestReceiverIgnoresNonSigilPayload(t *testing.T) {
	sigils := sigilcache.New()
	msg := &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{
		HelloReply: &keeperv1.HelloReply{SessionId: "s1"},
	}}

	dispatchPayload(msg, sigils, nil)

	if got := sigils.Get("core", "pkg"); got != nil {
		t.Fatalf("не-Sigil payload не должен наполнять кеш, получено %v", got)
	}
}

// genAnchorPEM generates a fresh ed25519 public key in SPKI PEM form
// (symmetric with keeper-side sigil.Signer.AnchorSetPEM) for anchor tests.
func genAnchorPEM(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	return pub, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func anchorsMsg(pems ...string) *keeperv1.FromKeeper {
	return &keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_SigilTrustAnchors{
		SigilTrustAnchors: &keeperv1.SigilTrustAnchors{PubkeyPem: pems},
	}}
}

// TestReceiverAppliesTrustAnchors — SigilTrustAnchors (R3-S6) replaces the
// anchor holder wholesale (ReplaceAll): the fresh set is parsed and applied.
func TestReceiverAppliesTrustAnchors(t *testing.T) {
	holder := sharedhost.NewAnchorSet(nil)
	_, pem1 := genAnchorPEM(t)
	_, pem2 := genAnchorPEM(t)

	dispatchPayload(anchorsMsg(pem1, pem2), nil, holder)

	parsed, err := parseTrustAnchorSet([]string{pem1, pem2})
	if err != nil {
		t.Fatalf("parseTrustAnchorSet: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("ожидали 2 якоря, распарсилось %d", len(parsed))
	}
}

// TestReceiverTrustAnchorsRejectsBrokenPEM — fail-closed: a broken PEM in the
// set does NOT replace the holder (the previous valid set is kept).
func TestReceiverTrustAnchorsRejectsBrokenPEM(t *testing.T) {
	prevPub, prevPEM := genAnchorPEM(t)
	holder := sharedhost.NewAnchorSet([]ed25519.PublicKey{prevPub})

	// Broken set: one valid PEM + one piece of garbage. parseTrustAnchorSet
	// returns an error → the holder is left untouched.
	_, goodPEM := genAnchorPEM(t)
	dispatchPayload(anchorsMsg(goodPEM, "not a pem block"), nil, holder)

	// The holder stays unchanged: verify by re-parsing the original set.
	keep, err := parseTrustAnchorSet([]string{prevPEM})
	if err != nil || len(keep) != 1 || !keep[0].Equal(prevPub) {
		t.Fatalf("предусловие/инвариант битого набора нарушен: keep=%d err=%v", len(keep), err)
	}
	if _, err := parseTrustAnchorSet([]string{goodPEM, "not a pem block"}); err == nil {
		t.Fatal("ожидали ошибку парсинга битого набора, получили nil")
	}
}

// TestReceiverEmptyTrustAnchorsClearsHolder — an empty set clears the holder
// (Sigil disabled on Keeper → verify fail-closed on no_trust_anchor).
func TestReceiverEmptyTrustAnchorsClearsHolder(t *testing.T) {
	pub, _ := genAnchorPEM(t)
	holder := sharedhost.NewAnchorSet([]ed25519.PublicKey{pub})

	dispatchPayload(anchorsMsg(), nil, holder)

	got, err := parseTrustAnchorSet(nil)
	if err != nil || got != nil {
		t.Fatalf("пустой набор должен парситься в nil без ошибки, got=%v err=%v", got, err)
	}
}

// TestReceiverTrustAnchorsNilHolder — without a holder (push / test without a
// Host) the branch is a no-op, no panic.
func TestReceiverTrustAnchorsNilHolder(t *testing.T) {
	_, pem1 := genAnchorPEM(t)
	dispatchPayload(anchorsMsg(pem1), nil, nil) // must not panic
}
