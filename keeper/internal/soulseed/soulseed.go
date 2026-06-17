// Package soulseed — типы реестра выпущенных SoulSeed-сертификатов
// (`soul_seeds`) под docs/soul/identity.md.
//
// В БД хранится только fingerprint (SHA-256 публичного ключа сертификата,
// hex). PEM, приватный ключ, серийник CA — не дублируются (главная защита —
// приватный ключ CA в Vault PKI).
//
// M2.1.a: типы + CRUD (Insert / SelectActiveBySID / SupersedeBySID / List).
// Vault PKI integration (signing CSR) и gRPC Bootstrap-handler — M2.1.b.
package soulseed

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"
)

// Status — состояние сертификата в реестре. Совпадает с CHECK
// `soul_seeds_status_valid` из 009_create_soul_seeds.up.sql
// (расширен миграцией 017 — добавлен `orphaned`).
//
//   - `active` — текущий выпущенный сертификат, ровно один per SID.
//   - `superseded` — заменён ротацией, новый seed уже active.
//   - `expired` — двинут Жнецом / Vault PKI после not_after.
//   - `revoked` — оператор отозвал (compromise), новые подключения отказываются.
//   - `orphaned` — хост cascade-удалён из `core.cloud.provisioned destroyed`
//     (ADR-017). Не перетирает `revoked` (приоритет revoked > orphaned).
type Status string

const (
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
	StatusExpired    Status = "expired"
	StatusRevoked    Status = "revoked"
	StatusOrphaned   Status = "orphaned"
)

// FingerprintHexLen — длина hex-представления SHA-256 fingerprint-а (64).
const FingerprintHexLen = 64

// Sentinel-ошибки для CRUD-слоя.
var (
	// ErrSeedInvalidFingerprint — fingerprint не соответствует формату
	// (64 lower-hex). Дублирует CHECK `soul_seeds_fingerprint_format`.
	ErrSeedInvalidFingerprint = errors.New("soulseed: fingerprint format invalid (must be 64 lower-hex chars)")
)

// SoulSeed — runtime-представление строки реестра `soul_seeds`.
type SoulSeed struct {
	SeedID           string    `json:"seed_id"`
	SID              string    `json:"sid"`
	Fingerprint      string    `json:"fingerprint"`
	SerialNumber     string    `json:"serial_number"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	IssuedByKID      *string   `json:"issued_by_kid,omitempty"`
	Status           Status    `json:"status"`
	RevocationReason *string   `json:"revocation_reason,omitempty"`
}

// FingerprintFromCert вычисляет SHA-256 fingerprint публичного ключа
// сертификата (DER form of SubjectPublicKeyInfo).
//
// Симметрично с тем, как `openssl x509 -pubkey | openssl dgst -sha256` —
// fingerprint привязан к ключу, а не к DER-сертификату целиком (последний
// меняется при перевыпуске того же ключа).
func FingerprintFromCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ValidFingerprintFormat — проверка формата (64 lower-hex). Caller
// валидирует до round-trip-а; PG CHECK страхует на стороне БД.
func ValidFingerprintFormat(fp string) bool {
	if len(fp) != FingerprintHexLen {
		return false
	}
	for _, c := range fp {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validStatus(s Status) bool {
	switch s {
	case StatusActive, StatusSuperseded, StatusExpired, StatusRevoked, StatusOrphaned:
		return true
	}
	return false
}
