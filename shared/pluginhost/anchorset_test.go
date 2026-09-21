package pluginhost

import (
	"bytes"
	"crypto/ed25519"
	"sync"
	"testing"
)

// TestAnchorSetSnapshotEmpty — a nil set and a nil holder return an empty snapshot
// (verify treats this as no_trust_anchor).
func TestAnchorSetSnapshotEmpty(t *testing.T) {
	if got := NewAnchorSet(nil).snapshot(); got != nil {
		t.Errorf("NewAnchorSet(nil).snapshot() = %v, want nil", got)
	}
	if got := NewAnchorSet([]ed25519.PublicKey{}).snapshot(); got != nil {
		t.Errorf("NewAnchorSet(empty).snapshot() = %v, want nil", got)
	}
	var nilHolder *AnchorSet
	if got := nilHolder.snapshot(); got != nil {
		t.Errorf("(*AnchorSet)(nil).snapshot() = %v, want nil", got)
	}
}

// TestAnchorSetSetReplaceAll — SetAnchors atomically replaces the whole set
// (ReplaceAll semantics, ADR-026(h)): old anchors are forgotten, new ones visible.
func TestAnchorSetSetReplaceAll(t *testing.T) {
	a := NewAnchorSet(genKeys(t, 2))
	if got := len(a.snapshot()); got != 2 {
		t.Fatalf("initial len = %d, want 2", got)
	}

	next := genKeys(t, 3)
	a.SetAnchors(next)
	snap := a.snapshot()
	if len(snap) != 3 {
		t.Fatalf("after replace len = %d, want 3", len(snap))
	}
	for i := range next {
		if !bytes.Equal(snap[i], next[i]) {
			t.Errorf("anchor %d mismatch after replace", i)
		}
	}

	a.SetAnchors(nil) // retire all anchors → empty set
	if got := a.snapshot(); got != nil {
		t.Errorf("after SetAnchors(nil) snapshot = %v, want nil", got)
	}
}

// TestAnchorSetSetCopiesInput — SetAnchors copies the header of the input slice:
// the caller mutating its buffer after the call must not affect the snapshot.
func TestAnchorSetSetCopiesInput(t *testing.T) {
	keys := genKeys(t, 2)
	a := NewAnchorSet(keys)

	orig := make([]byte, len(keys[0]))
	copy(orig, keys[0])
	keys[0] = keys[1] // swap an element in the caller's buffer

	snap := a.snapshot()
	if !bytes.Equal(snap[0], orig) {
		t.Errorf("snapshot[0] changed after mutating the caller buffer: the set was not copied")
	}
}

// TestAnchorSetRaceSnapshotDuringSet — a concurrent SetAnchors during
// snapshot/verify doesn't trip -race and doesn't return a corrupt snapshot. Under
// `go test -race` this test catches unsafe access to the holder.
func TestAnchorSetRaceSnapshotDuringSet(t *testing.T) {
	a := NewAnchorSet(genKeys(t, 1))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: continuously replaces the set (simulating S6 SigilTrustAnchors).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.SetAnchors(genKeys(t, 2))
			}
		}
	}()

	// Readers: continuously take a snapshot and read bytes (simulating verify).
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					for _, k := range a.snapshot() {
						_ = len(k) // touch — the race detector will see an unsafe read
					}
				}
			}
		}()
	}

	for i := 0; i < 1000; i++ {
		_ = a.snapshot()
	}
	close(stop)
	wg.Wait()
}

// TestVerifyAnyAnchorOR — OR helper: a signature is valid if at least one anchor
// in the set verifies it; none (or an empty set) → false.
func TestVerifyAnyAnchorOR(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	msg := []byte("sigil-block")
	sig := ed25519.Sign(priv, msg)

	other1, _, _ := ed25519.GenerateKey(nil)
	other2, _, _ := ed25519.GenerateKey(nil)

	cases := []struct {
		name    string
		anchors []ed25519.PublicKey
		want    bool
	}{
		{"empty", nil, false},
		{"only-signer", []ed25519.PublicKey{pub}, true},
		{"signer-among-foreign", []ed25519.PublicKey{other1, pub, other2}, true},
		{"all-foreign", []ed25519.PublicKey{other1, other2}, false},
		{"nil-key-skipped", []ed25519.PublicKey{nil, pub}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyAnyAnchor(tc.anchors, msg, sig); got != tc.want {
				t.Errorf("verifyAnyAnchor = %v, want %v", got, tc.want)
			}
		})
	}
}

func genKeys(t *testing.T, n int) []ed25519.PublicKey {
	t.Helper()
	out := make([]ed25519.PublicKey, n)
	for i := range out {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("genkey %d: %v", i, err)
		}
		out[i] = pub
	}
	return out
}
