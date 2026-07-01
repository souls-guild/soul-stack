// Package cert — реестр Warrant (`warrant`): выпущенные СЕРВИСНЫЕ TLS-серты
// инкарнаций под cert-rotation Вар1 (Keeper-центр). Ось скана Reaper-правила
// `rotate_due_certs` — `not_after`.
//
// ★ ОТЛИЧИЕ ОТ soulseed (`soul_seeds`, docs/soul/identity.md): soul_seeds —
// IDENTITY-серты Soul-агентов, где приватник НИКОГДА не покидает хост (в БД
// только fingerprint). Warrant — СЕРВИСНЫЕ серты (напр. серверный TLS Redis),
// где приватник ГЕНЕРИТСЯ Keeper-ом централизованно (R2, keeper/internal/vault/
// csrgen.go) и проходит через Keeper → Vault. Это осознанное исключение из
// инварианта identity-модели «приватник не покидает хост»: сервисный серт ≠
// identity-серт, и он уже лежит в Vault для ручного rotate_tls. Решение
// зафиксировано отдельным ADR cert-rotation. В БД приватник НЕ хранится —
// только vault_ref + fingerprint + serial.
//
// На одну (incarnation, kind) — много warrant-строк (история ротаций); один
// active каждого kind одновременно — гарантирует partial unique
// `warrant_active_by_incarnation_kind_idx`.
package cert

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"
)

// Kind — тип TLS-материала строки Warrant. Совпадает с CHECK
// `warrant_kind_valid` из 092_create_warrant.up.sql.
//
//   - `cert` — серверный сертификат (redis.crt).
//   - `key`  — приватный ключ (redis.key). not_after дублирует cert (одна пара).
//   - `ca`   — CA-сертификат кластера (ca.crt).
type Kind string

const (
	KindCert Kind = "cert"
	KindKey  Kind = "key"
	KindCA   Kind = "ca"
)

// Status — состояние строки в реестре. Совпадает с CHECK
// `warrant_status_valid`.
//
//   - `active`     — текущий выпущенный материал, ровно один per (incarnation, kind).
//   - `superseded` — заменён ротацией, новый active уже вписан.
//   - `expired`    — двинут после not_after (пост-MVP mover; сейчас — чистка purge).
//   - `rotating`   — CAS-помечен Reaper-правилом `rotate_due_certs` на время
//     цепочки ротации (single-winner-барьер против двойного спавна Voyage).
//   - `failed`     — цепочка ротации упала (SignCSR/WriteKV/Voyage-insert): строка
//     остаётся вне active, чтобы следующий тик не считал её due и не зациклился.
type Status string

const (
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
	StatusExpired    Status = "expired"
	StatusRotating   Status = "rotating"
	StatusFailed     Status = "failed"
)

// FingerprintHexLen — длина hex-представления SHA-256 fingerprint-а (64).
// Симметрия soulseed.FingerprintHexLen.
const FingerprintHexLen = 64

// Sentinel-ошибки CRUD-слоя.
var (
	// ErrInvalidFingerprint — fingerprint не 64 lower-hex. Дублирует CHECK
	// `warrant_fingerprint_format`.
	ErrInvalidFingerprint = errors.New("cert: fingerprint format invalid (must be 64 lower-hex chars)")
)

// Warrant — runtime-представление строки реестра `warrant`.
//
// Приватник и PEM здесь НЕ живут (защита — Vault, поле VaultRef). fingerprint
// SHA-256(SubjectPublicKeyInfo) — тот же класс, что soulseed.
type Warrant struct {
	CertID                  string         `json:"cert_id"`
	IncarnationID           string         `json:"incarnation_id"`
	Kind                    Kind           `json:"kind"`
	VaultRef                string         `json:"vault_ref"`
	SerialNumber            string         `json:"serial_number"`
	Fingerprint             string         `json:"fingerprint"`
	NotAfter                time.Time      `json:"not_after"`
	IssuedAt                time.Time      `json:"issued_at"`
	PKIMount                *string        `json:"pki_mount,omitempty"`
	PKIRole                 *string        `json:"pki_role,omitempty"`
	Status                  Status         `json:"status"`
	IssuedByKID             *string        `json:"issued_by_kid,omitempty"`
	LastRotationVoyageID    *string        `json:"last_rotation_voyage_id,omitempty"`
	AutoRotate              bool           `json:"auto_rotate"`
	RotateThresholdOverride *time.Duration `json:"rotate_threshold_override,omitempty"`
}

// FingerprintFromCert вычисляет SHA-256 fingerprint публичного ключа
// сертификата (DER SubjectPublicKeyInfo) — привязка к ключу, не к DER-серту
// целиком. Симметрия soulseed.FingerprintFromCert.
func FingerprintFromCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ValidFingerprintFormat — проверка формата (64 lower-hex). Caller валидирует
// до round-trip-а; PG CHECK страхует на стороне БД.
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

func validKind(k Kind) bool {
	switch k {
	case KindCert, KindKey, KindCA:
		return true
	}
	return false
}

func validStatus(s Status) bool {
	switch s {
	case StatusActive, StatusSuperseded, StatusExpired, StatusRotating, StatusFailed:
		return true
	}
	return false
}
