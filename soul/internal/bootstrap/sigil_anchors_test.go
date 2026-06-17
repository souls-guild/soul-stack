package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/soul/internal/seed"
)

// realPubPEM генерирует SPKI-PEM реального ed25519-pubkey (как пишет keeper-side
// Signer.PublicKeyPEM) — для проверки парса собранного набора.
func realPubPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// ptr — короткий хелпер для optional-поля sigil_pubkey_pem (proto oneof).
func ptr(s string) *string { return &s }

const (
	pemA = "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
	pemB = "-----BEGIN PUBLIC KEY-----\nBBBB\n-----END PUBLIC KEY-----\n"
)

// TestSigilAnchorsPEM_Priority — приоритет set > single (ADR-026(h)).
func TestSigilAnchorsPEM_Priority(t *testing.T) {
	tests := []struct {
		name   string
		single *string
		set    []string
		want   string // "" == nil (Sigil выключен)
	}{
		{
			name: "both empty -> nil (Sigil off)",
			want: "",
		},
		{
			name:   "set empty -> fallback to single (legacy)",
			single: ptr(pemA),
			want:   pemA,
		},
		{
			name: "set non-empty -> set wins, single ignored",
			// single задан, но при непустом set он должен игнорироваться.
			single: ptr(pemA),
			set:    []string{pemB},
			want:   pemB,
		},
		{
			name: "set multi-block concatenated",
			set:  []string{pemA, pemB},
			want: pemA + pemB,
		},
		{
			name:   "single empty string -> nil",
			single: ptr(""),
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := &keeperv1.BootstrapReply{
				SigilPubkeyPem:    tc.single,
				SigilPubkeyPemSet: tc.set,
			}
			got := sigilAnchorsPEM(reply)
			if string(got) != tc.want {
				t.Fatalf("sigilAnchorsPEM = %q; want %q", got, tc.want)
			}
			if tc.want == "" && got != nil {
				t.Fatalf("Sigil off must yield nil, got %q", got)
			}
		})
	}
}

// TestSigilAnchorsPEM_NormalizesBlockSeparators — элементы set без trailing \n
// конкатенируются с разделителем, чтобы seed.ParseSigilPubKeys увидел границы
// блоков (multi-PEM остаётся валидным).
func TestSigilAnchorsPEM_NormalizesBlockSeparators(t *testing.T) {
	noNL := "-----BEGIN PUBLIC KEY-----\nCCCC\n-----END PUBLIC KEY-----"
	reply := &keeperv1.BootstrapReply{
		SigilPubkeyPemSet: []string{noNL, pemA},
	}
	got := sigilAnchorsPEM(reply)
	want := noNL + "\n" + pemA
	if string(got) != want {
		t.Fatalf("separator not normalized:\n got %q\nwant %q", got, want)
	}
}

// TestSigilAnchorsPEM_RealKeysRoundTrip — собранный из реального multi-anchor
// набора PEM распарсивается seed.ParseSigilPubKeys обратно в N ключей.
func TestSigilAnchorsPEM_RealKeysRoundTrip(t *testing.T) {
	reply := &keeperv1.BootstrapReply{
		SigilPubkeyPemSet: []string{realPubPEM(t), realPubPEM(t)},
	}
	keys, err := seed.ParseSigilPubKeys(sigilAnchorsPEM(reply))
	if err != nil {
		t.Fatalf("ParseSigilPubKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 anchors, got %d", len(keys))
	}
}
