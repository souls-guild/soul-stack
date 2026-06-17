package sigil

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"regexp"

	"github.com/souls-guild/soul-stack/shared/pluginhost"
)

// reSHA256Hex — формат hex-отпечатка бинаря: ровно 64 нижних hex-символа.
// Совпадает с CHECK-constraint plugin_sigils.sha256 (миграция 028).
var reSHA256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Signer держит PRIMARY ed25519-приватник Keeper-а (которым подписываются новые
// блоки Sigil) и полный набор active trust-anchor-ов (публичные ключи всех
// active-ключей подписи, R3 multi-anchor, ADR-026(h)). Создаётся один раз на
// процесс при старте `keeper run` ([LoadSigner] из реестра sigil_signing_keys
// либо [NewSigner] single-fallback от cfg) и потокобезопасен для конкурентных
// Sign (ed25519.Sign не мутирует ключ, anchors immutable после конструктора).
//
// Подпись — всегда primary-ключом; anchors верифицируют (на Soul/keeper-host,
// S5/S6): любой anchor из набора валидирует подпись, что и даёт безразрывную
// ротацию (ADR-026(h)). primary всегда присутствует в anchors.
type Signer struct {
	priv    ed25519.PrivateKey  // PRIMARY: подписывает новые Sigil-ы
	anchors []ed25519.PublicKey // все active pubkey (включая primary), R3
}

// NewSigner оборачивает одиночный приватник в Signer (single-anchor режим:
// набор якорей = {primary}). priv обязан быть валидным ed25519-ключом полного
// размера (как возвращает [LoadSigningKey]); пустой/неверной длины ключ →
// ошибка, чтобы не подписывать мусором. Используется для cfg-fallback при
// пустом реестре ключей (setupSigil) и в тестах.
func NewSigner(priv ed25519.PrivateKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sigil: invalid ed25519 private key size %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{priv: priv, anchors: []ed25519.PublicKey{pub}}, nil
}

// NewMultiSigner собирает multi-anchor Signer: priv — PRIMARY-приватник (подпись),
// anchors — публичные ключи всех active-ключей подписи (R3 multi-anchor,
// ADR-026(h)). priv обязан быть валидным ed25519-ключом; пустой набор anchors
// → ошибка (verify лишился бы якорей). Публичная часть primary гарантированно
// включается в итоговый набор (даже если caller её не передал) — иначе Soul не
// смог бы верифицировать только что выпущенную primary-ключом подпись.
//
// Набор anchors дедуплицируется по сырым байтам ключа: реестр держит primary
// отдельной строкой, и её pubkey уже есть в anchors из pubkey_pem — повторно
// добавлять primary не нужно.
func NewMultiSigner(priv ed25519.PrivateKey, anchors []ed25519.PublicKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sigil: invalid ed25519 private key size %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	primaryPub := priv.Public().(ed25519.PublicKey)

	seen := make(map[string]struct{}, len(anchors)+1)
	set := make([]ed25519.PublicKey, 0, len(anchors)+1)
	add := func(pub ed25519.PublicKey) {
		if len(pub) != ed25519.PublicKeySize {
			return
		}
		k := string(pub)
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		set = append(set, pub)
	}
	add(primaryPub)
	for _, a := range anchors {
		add(a)
	}
	if len(set) == 0 {
		// primaryPub валиден (size проверен выше) → недостижимо; defensive.
		return nil, fmt.Errorf("sigil: empty anchor set")
	}
	return &Signer{priv: priv, anchors: set}, nil
}

// Public возвращает публичную часть PRIMARY-ключа. Едет Soul-у в bootstrap как
// одиночный trust-anchor (ADR-026(d), поле sigil_pubkey_pem); multi-anchor-набор
// для bootstrap (sigil_pubkey_pem_set) и runtime-broadcast — S6 ([AnchorSet]).
func (s *Signer) Public() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// AnchorSet возвращает публичные ключи всех active-ключей подписи (включая
// primary), R3 multi-anchor (ADR-026(h)). Это будущий набор для
// bootstrap-поля sigil_pubkey_pem_set и runtime-сообщения SigilTrustAnchors
// (ReplaceAll-семантика, безразрывная ротация) — раздача — S6.
//
// Возвращает копию слайса (заголовка): сами ed25519.PublicKey immutable, но
// слайс защищаем от мутации caller-ом. Порядок — как при сборке (primary
// первым в multi-режиме через [LoadSigner], где ListActiveKeys отдаёт primary
// первым).
func (s *Signer) AnchorSet() []ed25519.PublicKey {
	out := make([]ed25519.PublicKey, len(s.anchors))
	copy(out, s.anchors)
	return out
}

// PublicKeyPEM возвращает публичную часть подписывающего ключа в PEM-форме
// (SPKI: x509 MarshalPKIXPublicKey → pem-блок "PUBLIC KEY"). Этот PEM едет
// Soul-у в bootstrap-reply ([keeperv1.BootstrapReply.SigilPubkeyPem]) как
// trust-anchor для verify допусков плагинов (ADR-026, S6). Заполняется в
// [BootstrapDeps.SigilPubKeyPEM] при старте `keeper run`, только когда Sigil
// настроен. MarshalPKIXPublicKey для валидного ed25519-ключа не возвращает
// ошибку (size гарантирован [NewSigner]) — defensive-обёртка на случай
// будущих изменений.
func (s *Signer) PublicKeyPEM() ([]byte, error) {
	return publicKeyToPEM(s.priv.Public().(ed25519.PublicKey))
}

// AnchorSetPEM возвращает ПОЛНЫЙ набор trust-anchor-ов ([Signer.AnchorSet]) в
// PEM-форме — по одной SPKI-строке "PUBLIC KEY" на якорь, в том же порядке
// (primary первым). Это набор для bootstrap-поля
// [keeperv1.BootstrapReply.SigilPubkeyPemSet] (R3-S4 читает set>single) и для
// runtime-сообщения [keeperv1.SigilTrustAnchors] (broadcast + re-broadcast, S6).
//
// Каждая PEM-строка симметрична одиночному [Signer.PublicKeyPEM]: Soul парсит её
// тем же seed.ParseSigilPubKeys. Порядок сохраняется, чтобы primary оставался
// первым (бессмысленно для verify — OR по набору — но удобно для логов/диагностики).
func (s *Signer) AnchorSetPEM() ([]string, error) {
	out := make([]string, 0, len(s.anchors))
	for i, pub := range s.anchors {
		pemBytes, err := publicKeyToPEM(pub)
		if err != nil {
			return nil, fmt.Errorf("sigil: anchor %d to PEM: %w", i, err)
		}
		out = append(out, string(pemBytes))
	}
	return out, nil
}

// publicKeyToPEM кодирует ed25519-публичный ключ в SPKI PEM-блок "PUBLIC KEY".
// Общий код [Signer.PublicKeyPEM] (primary) и [Signer.AnchorSetPEM] (весь набор):
// форма обязана быть byte-идентичной, иначе Soul-side ParseSigilPubKeys разойдётся
// между bootstrap-single и runtime-set.
func publicKeyToPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: marshal public key (SPKI): %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Sign подписывает Sigil для плагина (namespace, name, ref) с бинарём
// binarySHA256hex и манифестом manifestBytes.
//
// Шаги (нормативный порядок, симметричный verify на Soul, S6):
//  1. binarySHA256Raw   = hex-decode(binarySHA256hex) — сырые 32 байта digest-а;
//  2. manifestSHA256Raw = SHA-256(NormalizeManifestBytes(manifestBytes)) —
//     S3↔S6-инвариант держится канонизацией перед хешем;
//  3. block             = BuildSigilBlock(...) — детерминированный блок с DST +
//     length-prefixed полями;
//  4. signature         = ed25519.Sign(priv, block) — сырые 64 байта.
//
// binarySHA256hex обязан быть 64 нижними hex-символами (формат digest-а
// бинаря). manifestBytes — СЫРЫЕ байты manifest.yaml как доставлены (без
// предварительной канонизации: её делает Sign).
func (s *Signer) Sign(namespace, name, ref, binarySHA256hex string, manifestBytes []byte) ([]byte, error) {
	if !reSHA256Hex.MatchString(binarySHA256hex) {
		return nil, fmt.Errorf("sigil: binary sha256 %q must be 64 lower-hex chars", binarySHA256hex)
	}
	binarySHA256Raw, err := hex.DecodeString(binarySHA256hex)
	if err != nil {
		// reSHA256Hex уже гарантирует валидный hex — defensive, не должно
		// случиться.
		return nil, fmt.Errorf("sigil: decode binary sha256: %w", err)
	}

	manifestDigest := sha256.Sum256(pluginhost.NormalizeManifestBytes(manifestBytes))
	block := pluginhost.BuildSigilBlock(namespace, name, ref, binarySHA256Raw, manifestDigest[:])

	return ed25519.Sign(s.priv, block), nil
}
