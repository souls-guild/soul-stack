package pluginhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// lookupStub — минимальный SigilLookup для тестов. nil-результат на отсутствие
// ключа моделирует «допуск не доехал».
type lookupStub map[string]*SigilRecord

func (l lookupStub) Get(ns, name string) *SigilRecord { return l[ns+"."+name] }

// signFixture симметрично keeper/internal/sigil.Signer.Sign собирает подпись
// над тем же блоком теми же helper-ами (BuildSigilBlock + NormalizeManifestBytes).
// Если verify и эта функция разойдутся — компилятор/тест это поймает: helper-ы
// общие, второй имплементации хеширования нет (S3↔S6-симметрия).
func signFixture(t *testing.T, priv ed25519.PrivateKey, ns, name, ref string, binDigestHex string, manifest []byte) []byte {
	t.Helper()
	binRaw, err := hex.DecodeString(binDigestHex)
	if err != nil {
		t.Fatalf("decode bin digest: %v", err)
	}
	manifestDigest := sha256.Sum256(NormalizeManifestBytes(manifest))
	block := BuildSigilBlock(ns, name, ref, binRaw, manifestDigest[:])
	return ed25519.Sign(priv, block)
}

// sigilTestEnv — собранный плагин на диске + согласованный валидный SigilRecord.
type sigilTestEnv struct {
	dir      string
	binPath  string
	manifest *sharedplugin.Manifest
	rec      *SigilRecord
	pub      ed25519.PublicKey
}

func setupSigilEnv(t *testing.T) sigilTestEnv {
	t.Helper()
	const (
		ns       = "wb"
		name     = "x"
		ref      = "v1.0.0"
		manifest = "kind: soul_module\nnamespace: wb\nname: x\nprotocol_version: 1\n"
	)
	dir := t.TempDir()
	binPath := filepath.Join(dir, "soul-mod-x")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	binDigest, err := computeFileDigest(binPath)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	sig := signFixture(t, priv, ns, name, ref, binDigest, []byte(manifest))
	return sigilTestEnv{
		dir:     dir,
		binPath: binPath,
		manifest: &sharedplugin.Manifest{
			Kind: sharedplugin.KindSoulModule, ProtocolVersion: 1, Namespace: ns, Name: name,
		},
		rec: &SigilRecord{
			Namespace:       ns,
			Name:            name,
			Ref:             ref,
			BinarySHA256hex: binDigest,
			Signature:       sig,
			Manifest:        []byte(manifest),
		},
		pub: pub,
	}
}

func (e sigilTestEnv) host(t *testing.T, withRec bool) *Host {
	t.Helper()
	h, err := NewHost(nil, filepath.Join(t.TempDir(), "sock"))
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{e.pub})
	look := lookupStub{}
	if withRec {
		look[e.rec.Namespace+"."+e.rec.Name] = e.rec
	}
	h.Sigils = look
	return h
}

func (e sigilTestEnv) discovered() Discovered {
	return Discovered{Manifest: e.manifest, BinaryPath: e.binPath, Dir: e.dir}
}

// asVerifyError извлекает *VerifyError из обёрнутой ошибки Spawn.
func asVerifyError(t *testing.T, err error) *VerifyError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrSigilVerify) {
		t.Fatalf("error %v is not ErrSigilVerify", err)
	}
	var ve *VerifyError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not *VerifyError", err)
	}
	return ve
}

// TestSigilVerifySuccess — валидный sigil + бинарь + manifest из транспорта:
// verify проходит, sidecar засилен. Spawn потом падает на handshake (бинарь —
// заглушка без handshake), но integrity-gate отработал ДО exec.
func TestSigilVerifySuccess(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	// Spawn упадёт после verify (нет handshake), но verify-этап не должен дать
	// VerifyError — проверяем именно отсутствие ErrSigilVerify.
	_, err := h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass for valid sigil, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); serr != nil {
		t.Fatalf("sidecar not sealed after verify-pass: %v", serr)
	}
}

func TestSigilVerifyNoSigil(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, false) // допуск не доехал

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoSigil {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoSigil)
	}
	if _, serr := os.Stat(filepath.Join(e.dir, DigestSidecarName)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must NOT be sealed on fail-closed, stat err = %v", serr)
	}
}

func TestSigilVerifyNoTrustAnchor(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	h.SigilAnchors = NewAnchorSet(nil) // пустой набор якорей: Sigil выключен на Keeper

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoTrustAnchor {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoTrustAnchor)
	}
}

// TestSigilVerifyNilAnchorHolder — nil-holder SigilAnchors (вообще не задан)
// эквивалентен пустому набору: verify fail-closed по no_trust_anchor (nil-safe
// snapshot). Покрывает обратную совместимость старого «nil pubkey».
func TestSigilVerifyNilAnchorHolder(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	h.SigilAnchors = nil

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonNoTrustAnchor {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonNoTrustAnchor)
	}
}

// TestSigilVerifyMultiAnchorOR — OR-цикл по набору якорей (ADR-026(h)): подпись
// выписана ОДНИМ ключом, но в наборе ещё посторонние якоря. verify проходит,
// если ключ-подписант присутствует в наборе среди прочих (безразрывная ротация).
func TestSigilVerifyMultiAnchorOR(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	otherPub1, _, _ := ed25519.GenerateKey(nil)
	otherPub2, _, _ := ed25519.GenerateKey(nil)
	// Набор: посторонний, ключ-подписант (e.pub), ещё посторонний. Ни порядок,
	// ни наличие чужих якорей не должны мешать — OR находит подписанта.
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{otherPub1, e.pub, otherPub2})

	_, err := h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("verify must pass when signer is one of the anchors, got %v", err)
	}
}

// TestSigilVerifyMultiAnchorAllForeign — непустой набор, но ключа-подписанта в
// нём НЕТ: ни один якорь не верифицирует → bad_signature (fail-closed). Это
// разделяет «пустой набор» (no_trust_anchor) и «есть якоря, но не тот»
// (bad_signature).
func TestSigilVerifyMultiAnchorAllForeign(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	f1, _, _ := ed25519.GenerateKey(nil)
	f2, _, _ := ed25519.GenerateKey(nil)
	h.SigilAnchors = NewAnchorSet([]ed25519.PublicKey{f1, f2})

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyDigestMismatch(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// Подмена бинаря после того, как допуск выписан под старый хеш.
	if err := os.WriteFile(e.binPath, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatalf("tamper bin: %v", err)
	}

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonDigestMismatch {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonDigestMismatch)
	}
}

func TestSigilVerifyBadSignatureManifestTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// Manifest в записи подменён после подписи — хеш manifest разойдётся с тем,
	// что в блоке подписи; digest бинаря при этом совпадает.
	e.rec.Manifest = []byte("kind: soul_module\nnamespace: wb\nname: x\nprotocol_version: 1\nside_effects: true\n")

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyBadSignatureCorrupted(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	e.rec.Signature = make([]byte, ed25519.SignatureSize) // нулевая подпись

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

func TestSigilVerifyRefTampered(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)
	// ref входит в подписываемый блок — его подмена ломает подпись.
	e.rec.Ref = "v9.9.9"

	_, err := h.Spawn(context.Background(), e.discovered())
	if ve := asVerifyError(t, err); ve.Reason != VerifyReasonBadSignature {
		t.Fatalf("reason = %q, want %q", ve.Reason, VerifyReasonBadSignature)
	}
}

// TestSigilSymmetryBlockMatchesSign — блок, который собирает verify, байт-в-байт
// совпадает с блоком, который подписывает Keeper Sign-flow: оба зовут
// BuildSigilBlock + NormalizeManifestBytes (общий код, не вторая имплементация).
func TestSigilSymmetryBlockMatchesSign(t *testing.T) {
	const (
		ns, name, ref = "core", "git", "v2.0.0"
		manifest      = "kind: soul_module\r\nnamespace: core\r\nname: git\r\n" // CRLF → нормализуется
	)
	binRaw := sha256.Sum256([]byte("binary-bytes"))
	binHex := hex.EncodeToString(binRaw[:])

	// Verify-сторона.
	manDigest := sha256.Sum256(NormalizeManifestBytes([]byte(manifest)))
	verifyBlock := BuildSigilBlock(ns, name, ref, binRaw[:], manDigest[:])

	// Sign-сторона воспроизводит ровно те же шаги (как keeper Sign).
	signBinRaw, _ := hex.DecodeString(binHex)
	signManDigest := sha256.Sum256(NormalizeManifestBytes([]byte(manifest)))
	signBlock := BuildSigilBlock(ns, name, ref, signBinRaw, signManDigest[:])

	if string(verifyBlock) != string(signBlock) {
		t.Fatalf("verify block != sign block:\n verify=%x\n sign  =%x", verifyBlock, signBlock)
	}
}

// TestSigilReExecBySidecar — после verify-pass последующий Spawn из кеша проходит
// integrity-gate по sidecar (re-exec defense-in-depth), даже когда допуск ещё в
// силе. Verify повторно сверяет sidecar, не пересоздавая его.
func TestSigilReExecBySidecar(t *testing.T) {
	e := setupSigilEnv(t)
	h := e.host(t, true)

	// Первый Spawn: verify-pass → seal.
	_, _ = h.Spawn(context.Background(), e.discovered())
	sidecar := filepath.Join(e.dir, DigestSidecarName)
	st1, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("sidecar after first spawn: %v", err)
	}

	// Второй Spawn: sidecar уже есть, verify сверяет его, не падает на verify.
	_, err = h.Spawn(context.Background(), e.discovered())
	if errors.Is(err, ErrSigilVerify) {
		t.Fatalf("re-exec must pass integrity, got %v", err)
	}
	st2, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("sidecar after second spawn: %v", err)
	}
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Errorf("sidecar rewritten on re-exec: mtime %v -> %v", st1.ModTime(), st2.ModTime())
	}
}
