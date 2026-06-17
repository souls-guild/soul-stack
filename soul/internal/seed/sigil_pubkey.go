package seed

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// ErrSigilPubKeyFormat — sigil_pubkey.pem есть, но не парсится в
// ed25519.PublicKey (не PEM / не SPKI / не ed25519-ключ). Trust-anchor битый —
// явная ошибка, а не молчаливое выключение verify (иначе подменённый/пустой
// файл тихо открыл бы fail-open).
var ErrSigilPubKeyFormat = errors.New("seed: sigil_pubkey.pem is not a valid ed25519 SPKI public key")

// ParseSigilPubKeys распознаёт НАБОР trust-anchor-ов Sigil из PEM-байт
// [Material.SigilPubKeyPEM]. Файл sigil_pubkey.pem может нести несколько
// PEM-блоков подряд (конкатенация) — multi-anchor для безразрывной ротации
// ключа подписи (ADR-026(h), R3). Каждый блок — SPKI ed25519, как пишет
// keeper-side sigil.Signer.PublicKeyPEM:
//
//   - пустой вход (Sigil выключен на Keeper) → (nil, nil): валидное состояние,
//     набор якорей пуст, verify плагинов fail-closed по no_trust_anchor;
//   - один блок → list длины 1 (обратная совместимость с single-anchor seed-ом);
//   - N блоков → N ключей в порядке записи;
//   - любой блок битый (не PEM / не-SPKI / RSA-ECDSA / лишний хвост) →
//     (nil, ErrSigilPubKeyFormat): caller обязан отказать в старте, а не
//     молча отключить verify (fail-closed на битом trust-anchor-е).
func ParseSigilPubKeys(pemBytes []byte) ([]ed25519.PublicKey, error) {
	if len(pemBytes) == 0 {
		return nil, nil
	}
	var keys []ed25519.PublicKey
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			// Нет ни одного блока вообще → битый вход. Уже распарсили хотя бы
			// один, а хвост не PEM → тоже отказ (не молчаливое усечение набора).
			if len(keys) == 0 {
				return nil, fmt.Errorf("%w: not a PEM block", ErrSigilPubKeyFormat)
			}
			if hasNonSpace(rest) {
				return nil, fmt.Errorf("%w: trailing data after %d PEM block(s)", ErrSigilPubKeyFormat, len(keys))
			}
			return keys, nil
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: parse SPKI (block %d): %v", ErrSigilPubKeyFormat, len(keys)+1, err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: SPKI key (block %d) is %T, want ed25519.PublicKey", ErrSigilPubKeyFormat, len(keys)+1, parsed)
		}
		keys = append(keys, pub)
	}
}

// hasNonSpace сообщает, есть ли в хвосте после pem.Decode значимые байты (не
// пробелы/переводы строк). Хвост из пустых строк между/после PEM-блоков —
// норма; непустой хвост = битый вход.
func hasNonSpace(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return true
		}
	}
	return false
}
