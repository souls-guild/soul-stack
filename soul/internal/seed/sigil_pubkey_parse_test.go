package seed

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
)

func ed25519PubPEM(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), pub
}

// TestParseSigilPubKeys_MultiBlocks — N конкатенированных PEM-блоков → N ключей
// в порядке записи (multi-anchor ротация, ADR-026(h) R3).
func TestParseSigilPubKeys_MultiBlocks(t *testing.T) {
	pem1, want1 := ed25519PubPEM(t)
	pem2, want2 := ed25519PubPEM(t)
	pem3, want3 := ed25519PubPEM(t)
	concat := append(append(append([]byte{}, pem1...), pem2...), pem3...)

	got, err := ParseSigilPubKeys(concat)
	if err != nil {
		t.Fatalf("ParseSigilPubKeys: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d", len(got))
	}
	for i, want := range []ed25519.PublicKey{want1, want2, want3} {
		if !got[i].Equal(want) {
			t.Fatalf("key[%d] mismatch", i)
		}
	}
}

// TestParseSigilPubKeys_SingleBlock — один блок → list длины 1 (обратная
// совместимость с single-anchor seed-ом).
func TestParseSigilPubKeys_SingleBlock(t *testing.T) {
	pemBytes, want := ed25519PubPEM(t)
	got, err := ParseSigilPubKeys(pemBytes)
	if err != nil {
		t.Fatalf("ParseSigilPubKeys single: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 key, got %d", len(got))
	}
	if !got[0].Equal(want) {
		t.Fatalf("single-block key mismatch")
	}
}

// TestParseSigilPubKeys_EmptyIsDisabled — пустой вход = Sigil выключен →
// (nil, nil), без ошибки.
func TestParseSigilPubKeys_EmptyIsDisabled(t *testing.T) {
	for _, in := range [][]byte{nil, {}} {
		got, err := ParseSigilPubKeys(in)
		if err != nil {
			t.Fatalf("empty must be (nil,nil), got err %v", err)
		}
		if got != nil {
			t.Fatalf("empty must yield nil slice, got %v", got)
		}
	}
}

// TestParseSigilPubKeys_NotPEM — мусор вместо PEM → ErrSigilPubKeyFormat.
func TestParseSigilPubKeys_NotPEM(t *testing.T) {
	_, err := ParseSigilPubKeys([]byte("not a pem block"))
	if !errors.Is(err, ErrSigilPubKeyFormat) {
		t.Fatalf("expected ErrSigilPubKeyFormat, got %v", err)
	}
}

// TestParseSigilPubKeys_TrailingGarbage — валидный блок + мусорный хвост →
// ErrSigilPubKeyFormat (не молчаливое усечение набора).
func TestParseSigilPubKeys_TrailingGarbage(t *testing.T) {
	pemBytes, _ := ed25519PubPEM(t)
	in := append(append([]byte{}, pemBytes...), []byte("garbage tail")...)
	_, err := ParseSigilPubKeys(in)
	if !errors.Is(err, ErrSigilPubKeyFormat) {
		t.Fatalf("expected ErrSigilPubKeyFormat on trailing garbage, got %v", err)
	}
}

// TestParseSigilPubKeys_WrongKeyTypeInSet — один из блоков не ed25519 → ошибка
// формата (fail-closed на битом якоре, а не выкидывание блока из набора).
func TestParseSigilPubKeys_WrongKeyTypeInSet(t *testing.T) {
	ed, _ := ed25519PubPEM(t)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey ecdsa: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal ecdsa SPKI: %v", err)
	}
	ec := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	_, err = ParseSigilPubKeys(append(append([]byte{}, ed...), ec...))
	if !errors.Is(err, ErrSigilPubKeyFormat) {
		t.Fatalf("expected ErrSigilPubKeyFormat for ECDSA in set, got %v", err)
	}
}
