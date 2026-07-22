package pluginhost

import (
	"crypto/ed25519"
	"sync/atomic"
)

// AnchorSet — an atomically swappable set of Sigil signing trust anchors
// (ADR-026(h), R3 multi-anchor). Holds a snapshot of the ed25519 public keys that
// verify checks the admission signature against: a signature is valid if ANY
// anchor in the set confirms it (OR semantics — this is what enables seamless
// signing-key rotation, see [verifySigilAndSeal]).
//
// Why atomic rather than a plain slice in Host: the set is updated at runtime by
// the SigilTrustAnchors message (ReplaceAll, S6) WITHOUT restarting the Soul, while
// concurrent Spawns read it in verify. atomic.Pointer over an immutable slice gives
// a lock-free snapshot for verify and atomic replacement for [AnchorSet.SetAnchors];
// ed25519.PublicKey values are immutable and the slice header is never mutated in
// place — only replaced wholesale.
//
// A nil (empty) set = Sigil disabled → verify fail-closed on no_trust_anchor (see
// [verifySigilAndSeal]). A nil *AnchorSet pointer is also valid and means "no
// anchors" — the methods are nil-safe.
type AnchorSet struct {
	// keys — immutable snapshot of the set. Written only wholesale (Store), read
	// lock-free (Load). nil = empty set.
	keys atomic.Pointer[[]ed25519.PublicKey]
}

// NewAnchorSet builds a set from initial anchors (the bootstrap set in S5). Copies
// the slice header so a caller mutation doesn't affect the snapshot. Empty/nil
// input → set with no anchors (verify fail-closed no_trust_anchor).
func NewAnchorSet(anchors []ed25519.PublicKey) *AnchorSet {
	a := &AnchorSet{}
	a.SetAnchors(anchors)
	return a
}

// SetAnchors atomically replaces the whole anchor set (ReplaceAll semantics,
// ADR-026(h)). Used by S6 when SigilTrustAnchors is delivered at runtime: after
// replacement concurrent verify sees the new set without a restart, an anchor
// outside the new set is "forgotten" (old key retired). Safe for concurrent use
// with [AnchorSet.snapshot].
//
// Copies the input slice header: the caller may reuse its buffer, the snapshot
// stays unchanged until the next SetAnchors.
func (a *AnchorSet) SetAnchors(anchors []ed25519.PublicKey) {
	if len(anchors) == 0 {
		a.keys.Store(nil)
		return
	}
	cp := make([]ed25519.PublicKey, len(anchors))
	copy(cp, anchors)
	a.keys.Store(&cp)
}

// snapshot returns the current immutable anchor set for verify. No copy: the slice
// is never mutated in place (SetAnchors swaps the whole pointer), so handing the
// internal slice to a read-only consumer is safe. nil = empty set → verify treats
// it as no_trust_anchor.
func (a *AnchorSet) snapshot() []ed25519.PublicKey {
	if a == nil {
		return nil
	}
	p := a.keys.Load()
	if p == nil {
		return nil
	}
	return *p
}
