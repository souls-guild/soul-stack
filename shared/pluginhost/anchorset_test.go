package pluginhost

import (
	"bytes"
	"crypto/ed25519"
	"sync"
	"testing"
)

// TestAnchorSetSnapshotEmpty — nil-набор и nil-holder отдают пустой snapshot
// (verify трактует это как no_trust_anchor).
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

// TestAnchorSetSetReplaceAll — SetAnchors атомарно заменяет весь набор
// (ReplaceAll-семантика ADR-026(h)): старые якоря забываются, новые видны.
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

	a.SetAnchors(nil) // retire всех якорей → пустой набор
	if got := a.snapshot(); got != nil {
		t.Errorf("after SetAnchors(nil) snapshot = %v, want nil", got)
	}
}

// TestAnchorSetSetCopiesInput — SetAnchors копирует заголовок входного слайса:
// мутация буфера caller-ом после вызова не должна затронуть снимок.
func TestAnchorSetSetCopiesInput(t *testing.T) {
	keys := genKeys(t, 2)
	a := NewAnchorSet(keys)

	orig := make([]byte, len(keys[0]))
	copy(orig, keys[0])
	keys[0] = keys[1] // подмена элемента в буфере caller-а

	snap := a.snapshot()
	if !bytes.Equal(snap[0], orig) {
		t.Errorf("snapshot[0] изменился после мутации caller-буфера: набор не скопирован")
	}
}

// TestAnchorSetRaceSnapshotDuringSet — конкурентный SetAnchors во время
// snapshot/verify не дёргает -race и не отдаёт битый снимок. Под `go test -race`
// этот тест ловит небезопасный доступ к holder-у.
func TestAnchorSetRaceSnapshotDuringSet(t *testing.T) {
	a := NewAnchorSet(genKeys(t, 1))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Писатель: непрерывно заменяет набор (имитация S6 SigilTrustAnchors).
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

	// Читатели: непрерывно берут snapshot и читают байты (имитация verify).
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
						_ = len(k) // touch — race-детектор увидит небезопасное чтение
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

// TestVerifyAnyAnchorOR — OR-helper: подпись валидна, если её подтвердил хотя бы
// один якорь набора; ни один (или пустой набор) → false.
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
