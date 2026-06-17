package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/pluginhost"
)

// genKeyPair генерирует ed25519-пару и её SPKI PEM (как пишет реестр в
// PubkeyPEM) + base64-raw приватник (форма Vault KV-значения).
func genKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	privB64 := base64.StdEncoding.EncodeToString(priv)
	return pub, priv, pubPEM, privB64
}

// fakeKVReader — in-memory KVReader: path → KV payload. Используется для мока
// Vault-чтения приватника в LoadSigner/LoadSigningKey.
type fakeKVReader struct {
	byPath map[string]map[string]any
}

func (f *fakeKVReader) ReadKV(_ context.Context, path string) (map[string]any, error) {
	kv, ok := f.byPath[path]
	if !ok {
		return nil, errors.New("fakeKVReader: path not found: " + path)
	}
	return kv, nil
}

// anchorContains проверяет наличие публичного ключа в наборе якорей.
func anchorContains(set []ed25519.PublicKey, want ed25519.PublicKey) bool {
	for _, a := range set {
		if a.Equal(want) {
			return true
		}
	}
	return false
}

// TestLoadSigner_MultiKey: реестр из primary + двух не-primary active-ключей →
// подписывает PRIMARY, AnchorSet содержит публичные ключи всех трёх. Приватники
// не-primary ключей из Vault НЕ читаются (в fake лежит только primary).
func TestLoadSigner_MultiKey(t *testing.T) {
	pubP, privP, pemP, privPB64 := genKeyPair(t)
	pubA, _, pemA, _ := genKeyPair(t)
	pubB, _, pemB, _ := genKeyPair(t)

	// ListActiveKeys отдаёт primary первым (ORDER BY is_primary DESC).
	keys := []*SigningKey{
		{KeyID: "kid-primary", PubkeyPEM: pemP, VaultRef: "vault:secret/keeper/sigil-primary", IsPrimary: true, Status: "active"},
		{KeyID: "kid-a", PubkeyPEM: pemA, VaultRef: "vault:secret/keeper/sigil-a", IsPrimary: false, Status: "active"},
		{KeyID: "kid-b", PubkeyPEM: pemB, VaultRef: "vault:secret/keeper/sigil-b", IsPrimary: false, Status: "active"},
	}
	vc := &fakeKVReader{byPath: map[string]map[string]any{
		// Только primary-приватник доступен — доказывает, что LoadSigner НЕ
		// читает приватники не-primary ключей.
		"secret/keeper/sigil-primary": {"signing_key": privPB64},
	}}

	signer, err := LoadSigner(context.Background(), vc, keys)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	// Подпись — primary-ключом: проверяем verify публичной частью primary.
	if !signer.Public().Equal(pubP) {
		t.Error("signer.Public() != primary pubkey")
	}
	if !signer.priv.Equal(privP) {
		t.Error("signer private key != primary private key")
	}

	set := signer.AnchorSet()
	if len(set) != 3 {
		t.Fatalf("AnchorSet len = %d, want 3", len(set))
	}
	for name, pub := range map[string]ed25519.PublicKey{"primary": pubP, "a": pubA, "b": pubB} {
		if !anchorContains(set, pub) {
			t.Errorf("AnchorSet missing %s pubkey", name)
		}
	}
	// primary должен идти первым (порядок ListActiveKeys).
	if !set[0].Equal(pubP) {
		t.Error("AnchorSet[0] is not primary")
	}
}

// TestLoadSigner_SignUsesPrimary: выпущенная подпись верифицируется ИМЕННО
// primary-публичным ключом (а не другим anchor-ом).
func TestLoadSigner_SignUsesPrimary(t *testing.T) {
	pubP, _, pemP, privPB64 := genKeyPair(t)
	pubA, _, pemA, _ := genKeyPair(t)

	keys := []*SigningKey{
		{KeyID: "kid-primary", PubkeyPEM: pemP, VaultRef: "vault:secret/keeper/p", IsPrimary: true, Status: "active"},
		{KeyID: "kid-a", PubkeyPEM: pemA, VaultRef: "vault:secret/keeper/a", IsPrimary: false, Status: "active"},
	}
	vc := &fakeKVReader{byPath: map[string]map[string]any{
		"secret/keeper/p": {"signing_key": privPB64},
	}}

	signer, err := LoadSigner(context.Background(), vc, keys)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	ns, name, ref := "cloud", "hetzner", "v1.0.0"
	binDigest := sha256.Sum256([]byte("plugin-binary"))
	binHex := hex.EncodeToString(binDigest[:])
	manifest := []byte("kind: cloud_driver\n")

	sig, err := signer.Sign(ns, name, ref, binHex, manifest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	manDigest := sha256.Sum256(pluginhost.NormalizeManifestBytes(manifest))
	block := pluginhost.BuildSigilBlock(ns, name, ref, binDigest[:], manDigest[:])

	if !ed25519.Verify(pubP, block, sig) {
		t.Error("primary pubkey failed to verify signature")
	}
	if ed25519.Verify(pubA, block, sig) {
		t.Error("non-primary anchor verified signature — Sign did not use primary")
	}
}

// TestLoadSigner_Empty: пустой active-набор → ErrKeyNotFound (caller-fallback на
// cfg решается в daemon, см. buildSigilSigner).
func TestLoadSigner_Empty(t *testing.T) {
	vc := &fakeKVReader{byPath: map[string]map[string]any{}}
	if _, err := LoadSigner(context.Background(), vc, nil); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("LoadSigner(nil) err = %v, want ErrKeyNotFound", err)
	}
	if _, err := LoadSigner(context.Background(), vc, []*SigningKey{}); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("LoadSigner([]) err = %v, want ErrKeyNotFound", err)
	}
}

// TestLoadSigner_FallbackEquivalence: single-anchor cfg-fallback ([NewSigner])
// даёт тот же signer-контракт, что LoadSigner с единственным primary-ключом —
// AnchorSet из одного ключа, подпись им же. Документирует решение «пустой реестр
// → работаем от cfg как single-anchor».
func TestLoadSigner_FallbackEquivalence(t *testing.T) {
	_, priv, pemP, privB64 := genKeyPair(t)

	// Путь A: одиночный cfg-fallback.
	cfgSigner, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if len(cfgSigner.AnchorSet()) != 1 {
		t.Fatalf("cfg-fallback AnchorSet len = %d, want 1", len(cfgSigner.AnchorSet()))
	}

	// Путь B: реестр из ровно одного primary-ключа.
	vc := &fakeKVReader{byPath: map[string]map[string]any{
		"secret/keeper/only": {"signing_key": privB64},
	}}
	regSigner, err := LoadSigner(context.Background(), vc, []*SigningKey{
		{KeyID: "kid-only", PubkeyPEM: pemP, VaultRef: "vault:secret/keeper/only", IsPrimary: true, Status: "active"},
	})
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	if !cfgSigner.Public().Equal(regSigner.Public()) {
		t.Error("cfg-fallback and single-key registry signers have different primary")
	}
	if len(regSigner.AnchorSet()) != 1 {
		t.Errorf("single-key registry AnchorSet len = %d, want 1", len(regSigner.AnchorSet()))
	}
}

// TestLoadSigner_NoPrimary: active-набор без primary → ErrKeyNotFound (некому
// подписывать; реестр-инвариант нарушен, fail-closed).
func TestLoadSigner_NoPrimary(t *testing.T) {
	_, _, pemA, _ := genKeyPair(t)
	vc := &fakeKVReader{byPath: map[string]map[string]any{}}
	keys := []*SigningKey{
		{KeyID: "kid-a", PubkeyPEM: pemA, VaultRef: "vault:secret/keeper/a", IsPrimary: false, Status: "active"},
	}
	if _, err := LoadSigner(context.Background(), vc, keys); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("LoadSigner(no primary) err = %v, want ErrKeyNotFound", err)
	}
}

// TestLoadSigner_BadAnchorPEM: битый pubkey_pem в реестре → ErrAnchorPubkeyFormat
// (fail-closed, не молчаливый пропуск якоря).
func TestLoadSigner_BadAnchorPEM(t *testing.T) {
	_, _, pemP, privB64 := genKeyPair(t)
	vc := &fakeKVReader{byPath: map[string]map[string]any{
		"secret/keeper/p": {"signing_key": privB64},
	}}
	keys := []*SigningKey{
		{KeyID: "kid-primary", PubkeyPEM: pemP, VaultRef: "vault:secret/keeper/p", IsPrimary: true, Status: "active"},
		{KeyID: "kid-broken", PubkeyPEM: "-----BEGIN PUBLIC KEY-----\nnot-base64\n-----END PUBLIC KEY-----\n", VaultRef: "vault:secret/keeper/x", IsPrimary: false, Status: "active"},
	}
	if _, err := LoadSigner(context.Background(), vc, keys); !errors.Is(err, ErrAnchorPubkeyFormat) {
		t.Errorf("LoadSigner(broken anchor) err = %v, want ErrAnchorPubkeyFormat", err)
	}
}

// TestNewMultiSigner_PrimaryAlwaysIncluded: даже если caller не передал
// публичную часть primary в anchors, она добавляется (иначе Soul не верифицирует
// свежую подпись).
func TestNewMultiSigner_PrimaryAlwaysIncluded(t *testing.T) {
	pubP, privP, _, _ := genKeyPair(t)
	pubA, _, _, _ := genKeyPair(t)

	signer, err := NewMultiSigner(privP, []ed25519.PublicKey{pubA}) // primary НЕ в списке
	if err != nil {
		t.Fatalf("NewMultiSigner: %v", err)
	}
	set := signer.AnchorSet()
	if !anchorContains(set, pubP) {
		t.Error("primary pubkey not auto-included in AnchorSet")
	}
	if !anchorContains(set, pubA) {
		t.Error("provided anchor missing from AnchorSet")
	}
	if len(set) != 2 {
		t.Errorf("AnchorSet len = %d, want 2", len(set))
	}
}

// TestNewMultiSigner_Dedup: дубль primary в anchors (реестр держит primary
// отдельной строкой, её pubkey уже в anchors) не двоится.
func TestNewMultiSigner_Dedup(t *testing.T) {
	pubP, privP, _, _ := genKeyPair(t)
	signer, err := NewMultiSigner(privP, []ed25519.PublicKey{pubP, pubP})
	if err != nil {
		t.Fatalf("NewMultiSigner: %v", err)
	}
	if got := len(signer.AnchorSet()); got != 1 {
		t.Errorf("AnchorSet len = %d, want 1 (deduped)", got)
	}
}

// TestNewMultiSigner_RejectsBadPrimary: невалидный primary-приватник → ошибка.
func TestNewMultiSigner_RejectsBadPrimary(t *testing.T) {
	if _, err := NewMultiSigner(ed25519.PrivateKey([]byte("short")), nil); err == nil {
		t.Error("NewMultiSigner accepted undersized primary key")
	}
}

// TestAnchorSet_ReturnsCopy: мутация возвращённого слайса не трогает Signer.
func TestAnchorSet_ReturnsCopy(t *testing.T) {
	_, priv, _, _ := genKeyPair(t)
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	set := signer.AnchorSet()
	set[0] = nil
	if signer.AnchorSet()[0] == nil {
		t.Error("AnchorSet returned a shared slice — internal state mutated")
	}
}

// TestAnchorSetPEM_MatchesAnchorSet: PEM-набор соответствует AnchorSet по числу,
// порядку и round-trip-у (каждый PEM парсится обратно в тот же публичный ключ).
// Это набор для bootstrap-set и runtime SigilTrustAnchors (R3-S6).
func TestAnchorSetPEM_MatchesAnchorSet(t *testing.T) {
	pubP, privP, pemP, privPB64 := genKeyPair(t)
	pubA, _, pemA, _ := genKeyPair(t)

	keys := []*SigningKey{
		{KeyID: "kid-primary", PubkeyPEM: pemP, VaultRef: "vault:secret/keeper/p", IsPrimary: true, Status: "active"},
		{KeyID: "kid-a", PubkeyPEM: pemA, VaultRef: "vault:secret/keeper/a", IsPrimary: false, Status: "active"},
	}
	vc := &fakeKVReader{byPath: map[string]map[string]any{
		"secret/keeper/p": {"signing_key": privPB64},
	}}
	signer, err := LoadSigner(context.Background(), vc, keys)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}

	pemSet, err := signer.AnchorSetPEM()
	if err != nil {
		t.Fatalf("AnchorSetPEM: %v", err)
	}
	set := signer.AnchorSet()
	if len(pemSet) != len(set) {
		t.Fatalf("AnchorSetPEM len = %d, want %d", len(pemSet), len(set))
	}
	for i, p := range pemSet {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			t.Fatalf("anchor %d: not a PEM block", i)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			t.Fatalf("anchor %d: parse SPKI: %v", i, err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			t.Fatalf("anchor %d: not ed25519", i)
		}
		if !pub.Equal(set[i]) {
			t.Errorf("anchor %d: PEM round-trip != AnchorSet[%d]", i, i)
		}
	}
	_ = privP
	// primary остаётся первым и присутствует в наборе.
	firstBlock, _ := pem.Decode([]byte(pemSet[0]))
	firstParsed, _ := x509.ParsePKIXPublicKey(firstBlock.Bytes)
	if !firstParsed.(ed25519.PublicKey).Equal(pubP) {
		t.Error("AnchorSetPEM[0] is not primary")
	}
	_ = pubA
}

// TestAnchorSetPEM_PrimaryMatchesPublicKeyPEM: для primary первая PEM-строка
// набора byte-идентична одиночному [Signer.PublicKeyPEM] (single-anchor seed и
// первый элемент multi-набора совпадают — Soul парсит обе тем же кодом).
func TestAnchorSetPEM_PrimaryMatchesPublicKeyPEM(t *testing.T) {
	_, priv, _, _ := genKeyPair(t)
	signer, err := NewSigner(priv) // single-anchor: набор = {primary}
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	single, err := signer.PublicKeyPEM()
	if err != nil {
		t.Fatalf("PublicKeyPEM: %v", err)
	}
	pemSet, err := signer.AnchorSetPEM()
	if err != nil {
		t.Fatalf("AnchorSetPEM: %v", err)
	}
	if len(pemSet) != 1 {
		t.Fatalf("single-anchor AnchorSetPEM len = %d, want 1", len(pemSet))
	}
	if pemSet[0] != string(single) {
		t.Errorf("AnchorSetPEM[0] != PublicKeyPEM:\n set=%q\n single=%q", pemSet[0], single)
	}
}

// TestSigner_NoPrivateKeyLeak: ни AnchorSet, ни Public, ни PublicKeyPEM не
// раскрывают приватник. Покрывает security-инвариант «приватник в памяти/Vault,
// НЕ в публичных surface-ах» (log-leak ловится тем, что Signer не имеет String()
// и приватник недоступен снаружи пакета).
func TestSigner_NoPrivateKeyLeak(t *testing.T) {
	pub, priv, _, _ := genKeyPair(t)
	signer, err := NewSigner(priv)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Публичные accessor-ы отдают только публичный материал.
	if !signer.Public().Equal(pub) {
		t.Fatal("Public() mismatch")
	}
	for _, a := range signer.AnchorSet() {
		// anchor никогда не должен совпадать по длине с приватником.
		if len(a) == ed25519.PrivateKeySize {
			t.Error("AnchorSet entry has private-key size — possible private leak")
		}
		// и не должен начинаться с seed-байтов приватника.
		seed := priv.Seed()
		if len(a) >= len(seed) && string(a[:len(seed)]) == string(seed) {
			t.Error("AnchorSet entry exposes private seed bytes")
		}
	}
}
