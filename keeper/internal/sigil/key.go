// Package sigil — keeper-side подпись бинарей плагинов и реестр допусков
// (ADR-026, slice S3, печать доверия Sigil).
//
// Состав пакета:
//   - key.go    — загрузка ed25519-приватника подписи из Vault KV;
//   - sign.go   — сборка хеша manifest+бинарь и ed25519-подпись блока Sigil;
//   - store.go  — CRUD реестра plugin_sigils (allow / revoke / list / lookup).
//
// S3↔S6-инвариант (нормативный, держится этим пакетом совместно с
// shared/pluginhost.NormalizeManifestBytes): байты manifest.yaml, которые
// Keeper хеширует при [Signer.Sign], ДОЛЖНЫ быть теми же, что Soul re-хеширует
// при verify (S6). Гарантия — (1) manifest+бинарь доставляются одним
// artifact-потоком, (2) обе стороны прогоняют сырые байты через
// NormalizeManifestBytes перед SHA-256. Подписываемый блок собирает чистая
// детерминированная shared/pluginhost.BuildSigilBlock — общий код для Sign (S3)
// и Verify (S6), без proto-marshal.
//
// Асимметрия ключей обязательна: подпись Sigil — ed25519 (приватник на Keeper,
// публичный ключ едет Soul-у в bootstrap). Это НЕ HS256-симметричный signing-key
// JWT (ADR-014); extractor здесь — отдельный, ed25519-специфичный.
package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// vaultSigningKeyField — имя поля внутри Vault KV-secret, в котором лежит
// ed25519-приватник подписи Sigil. Совпадает с конвенцией JWT signing-key
// (поле `signing_key`), чтобы local-dev `vault kv put`-команда и golden-фикстуры
// были единообразны; путь к секрету различает назначение (sigil vs jwt).
const vaultSigningKeyField = "signing_key"

// ErrSigningKeyMissing — в Vault KV нет поля `signing_key` либо оно пустое.
var ErrSigningKeyMissing = errors.New("sigil: signing_key field missing or empty in Vault KV")

// ErrSigningKeyFormat — поле `signing_key` есть, но не парсится в
// ed25519.PrivateKey ни в одной из поддерживаемых форм (см. parseEd25519Key).
// Невалидный/HS256-формат → эта ошибка, а не молчаливое игнорирование.
var ErrSigningKeyFormat = errors.New("sigil: signing_key is not a valid ed25519 private key")

// ErrAnchorPubkeyFormat — pubkey_pem строки реестра sigil_signing_keys не
// парсится в ed25519.PublicKey (SPKI PEM). Реестр пишет PubkeyPEM сам (через
// [Signer.PublicKeyPEM] на стороне Introduce), так что в норме недостижимо —
// fail-closed на случай ручной порчи строки в БД.
var ErrAnchorPubkeyFormat = errors.New("sigil: anchor pubkey_pem is not a valid ed25519 public key (SPKI PEM)")

// KVReader — узкое подмножество [keepervault.Client], нужное загрузке ключей
// подписи: одно чтение KV-secret-а по logical-path. Сужение позволяет
// unit-тестировать multi-key load через fake-reader; реальный
// *keepervault.Client удовлетворяет автоматически (симметрично ExecQueryRower).
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

var _ KVReader = (*keepervault.Client)(nil)

// LoadSigningKey читает ed25519-приватник подписи Sigil из Vault KV по
// config-ссылке `sigil.signing_key_ref` (`vault:<mount>/<path>`).
//
// Паттерн ParseRef + ReadKV симметричен bootstrap.LoadSigningKey, НО extractor
// — ed25519-специфичный (parseEd25519Key), а не HS256-raw-bytes: формы ключей
// несовместимы, переиспользовать extractSigningKey нельзя.
//
// Возврат nil-safe для caller-ов S3: при пустом ref / nil-клиенте — ошибка, не
// паника. Отсутствие ключа в конфиге обрабатывает caller (S4 вернёт «sigil key
// not configured»); сюда пустой ref не должен доходить.
func LoadSigningKey(ctx context.Context, vc KVReader, signingKeyRef string) (ed25519.PrivateKey, error) {
	if vc == nil {
		return nil, fmt.Errorf("sigil: vault client is nil")
	}
	if signingKeyRef == "" {
		return nil, fmt.Errorf("sigil: signing_key_ref is empty")
	}
	path, err := keepervault.ParseRef(signingKeyRef)
	if err != nil {
		return nil, fmt.Errorf("sigil: signing_key_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("sigil: read vault %q: %w", path, err)
	}
	return extractEd25519Key(kv)
}

// LoadSigner собирает multi-anchor [Signer] из active-набора реестра
// sigil_signing_keys (R3, ADR-026(h)):
//
//   - primary-ключ (keys[i].IsPrimary) → его приватник читается из Vault по
//     VaultRef и становится подписывающим ключом;
//   - набор anchors = публичные ключи (PubkeyPEM) ВСЕХ active-ключей.
//
// Приватники НЕ-primary ключей НЕ читаются: для подписи нужен только primary, а
// для verify достаточно публичных частей из реестра. Это сужает экспозицию
// приватников (читаем из Vault ровно один) и совпадает с моделью ADR-026(h),
// где anchors раздаются Soul-у публичными ключами.
//
// keys ожидаются как результат [ListActiveKeys] (порядок — primary первым).
// Ровно один primary гарантируется инвариантом реестра (keys.go); здесь —
// fail-closed-проверки: пустой набор → [ErrKeyNotFound], нет primary →
// [ErrKeyNotFound] (некому подписывать). Caller (setupSigil) на пустой реестр
// решает fallback на cfg, см. daemon.
func LoadSigner(ctx context.Context, vc KVReader, keys []*SigningKey) (*Signer, error) {
	if vc == nil {
		return nil, fmt.Errorf("sigil: vault client is nil")
	}
	if len(keys) == 0 {
		return nil, ErrKeyNotFound
	}

	var primary *SigningKey
	anchors := make([]ed25519.PublicKey, 0, len(keys))
	for _, k := range keys {
		pub, err := parseEd25519PublicPEM(k.PubkeyPEM)
		if err != nil {
			return nil, fmt.Errorf("sigil: anchor key_id=%s: %w", k.KeyID, err)
		}
		anchors = append(anchors, pub)
		if k.IsPrimary {
			if primary != nil {
				// Инвариант «ровно один primary» нарушен — реестр битый,
				// fail-closed (не выбирать произвольный).
				return nil, fmt.Errorf("sigil: more than one primary key in active set (key_id=%s and %s)", primary.KeyID, k.KeyID)
			}
			primary = k
		}
	}
	if primary == nil {
		return nil, ErrKeyNotFound
	}

	priv, err := LoadSigningKey(ctx, vc, primary.VaultRef)
	if err != nil {
		return nil, fmt.Errorf("sigil: load primary private key (key_id=%s): %w", primary.KeyID, err)
	}
	return NewMultiSigner(priv, anchors)
}

// parseEd25519PublicPEM парсит SPKI PEM-блок ("PUBLIC KEY") в ed25519.PublicKey.
// Не-ed25519 (RSA/ECDSA) или нечитаемый PEM → [ErrAnchorPubkeyFormat].
func parseEd25519PublicPEM(pemStr string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("%w: not a PEM block", ErrAnchorPubkeyFormat)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse SPKI: %v", ErrAnchorPubkeyFormat, err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: SPKI key is %T, want ed25519.PublicKey", ErrAnchorPubkeyFormat, parsed)
	}
	return pub, nil
}

// extractEd25519Key достаёт поле `signing_key` из Vault KV payload-а и парсит
// его в ed25519.PrivateKey. Поддерживаемые формы значения:
//
//   - string, PEM (`-----BEGIN PRIVATE KEY-----`, PKCS#8) → парсится x509;
//   - string, base64 от PKCS#8 DER → декодируется и парсится x509;
//   - string, base64 от raw 64-байтного ed25519 seed||pub (формат
//     ed25519.PrivateKey) → используется напрямую;
//   - string, base64 от raw 32-байтного seed → ed25519.NewKeyFromSeed;
//   - []byte с теми же raw-вариантами (64 / 32 байта).
//
// Любая другая форма (в т.ч. короткий HS256-secret произвольной длины) →
// [ErrSigningKeyFormat]. Отсутствует/пусто → [ErrSigningKeyMissing].
func extractEd25519Key(kv map[string]any) (ed25519.PrivateKey, error) {
	raw, ok := kv[vaultSigningKeyField]
	if !ok {
		return nil, ErrSigningKeyMissing
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil, ErrSigningKeyMissing
		}
		return parseEd25519Key([]byte(v))
	case []byte:
		if len(v) == 0 {
			return nil, ErrSigningKeyMissing
		}
		return parseEd25519Key(v)
	default:
		return nil, fmt.Errorf("sigil: signing_key has unsupported type %T (want string or []byte)", raw)
	}
}

// parseEd25519Key пробует распознать ed25519-приватник из набора форм по
// порядку: PEM → base64(DER/raw) → raw-байты. Возвращает [ErrSigningKeyFormat],
// если ни одна форма не подошла (включая валидный, но не-ed25519 PKCS#8-ключ —
// например RSA/ECDSA).
func parseEd25519Key(b []byte) (ed25519.PrivateKey, error) {
	// (1) PEM-обёртка (PKCS#8).
	if block, _ := pem.Decode(b); block != nil {
		key, err := parsePKCS8Ed25519(block.Bytes)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	// (2) base64 → DER (PKCS#8) либо raw-байты.
	if decoded, err := base64.StdEncoding.DecodeString(string(b)); err == nil {
		if key := tryRawEd25519(decoded); key != nil {
			return key, nil
		}
		if key, err := parsePKCS8Ed25519(decoded); err == nil {
			return key, nil
		}
		// base64 декодировался, но содержимое — не ed25519: явная ошибка
		// формата, не падать в raw-ветку (это привело бы к мусорному ключу).
		return nil, fmt.Errorf("%w: base64 payload is neither raw ed25519 (32/64 bytes) nor PKCS#8 ed25519", ErrSigningKeyFormat)
	}

	// (3) сырые байты (не-base64 значение KV).
	if key := tryRawEd25519(b); key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("%w: value is not PEM, base64 or raw ed25519 key", ErrSigningKeyFormat)
}

// tryRawEd25519 интерпретирует байты как сырой ed25519-ключ: 64 байта =
// seed||pub (ed25519.PrivateKey как есть), 32 байта = seed (через
// NewKeyFromSeed). Иначе nil — caller решает, ошибка это или повод пробовать
// другую форму.
func tryRawEd25519(b []byte) ed25519.PrivateKey {
	switch len(b) {
	case ed25519.PrivateKeySize: // 64
		key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
		copy(key, b)
		return key
	case ed25519.SeedSize: // 32
		return ed25519.NewKeyFromSeed(b)
	default:
		return nil
	}
}

// parsePKCS8Ed25519 парсит PKCS#8 DER и проверяет, что внутри именно
// ed25519-ключ. RSA/ECDSA-ключ в PKCS#8 → [ErrSigningKeyFormat].
func parsePKCS8Ed25519(der []byte) (ed25519.PrivateKey, error) {
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: parse PKCS#8: %v", ErrSigningKeyFormat, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: PKCS#8 key is %T, want ed25519.PrivateKey", ErrSigningKeyFormat, parsed)
	}
	return key, nil
}
