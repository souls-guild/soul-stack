// Package soulseed contains registry types for issued SoulSeed certificates
// (`soul_seeds`) under docs/soul/identity.md.
//
// DB stores only fingerprint (certificate public-key SHA-256, hex). PEM,
// private key, CA serial are not duplicated (primary protection is the CA
// private key in Vault PKI).
//
// M2.1.a: types + CRUD (Insert / SelectActiveBySID / SupersedeBySID / List).
// Vault PKI integration (signing CSR) and gRPC Bootstrap handler are M2.1.b.
package soulseed

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"time"
)

// Status is certificate state in registry. It matches CHECK
// `soul_seeds_status_valid` from 009_create_soul_seeds.up.sql (extended by
// migration 017 with `orphaned`).
//
//   - `active` is current issued certificate, exactly one per SID.
//   - `superseded` was replaced by rotation, new seed is already active.
//   - `expired` was moved by Reaper / Vault PKI after not_after.
//   - `revoked` was revoked by operator (compromise), new connections are denied.
//   - `orphaned` means host was cascade-deleted from `core.cloud.provisioned destroyed`
//     (ADR-017). It does not overwrite `revoked` (revoked > orphaned priority).
type Status string

const (
	StatusActive     Status = "active"
	StatusSuperseded Status = "superseded"
	StatusExpired    Status = "expired"
	StatusRevoked    Status = "revoked"
	StatusOrphaned   Status = "orphaned"
)

// FingerprintHexLen is length of SHA-256 fingerprint hex representation (64).
const FingerprintHexLen = 64

// Sentinel errors for the CRUD layer.
var (
	// ErrSeedInvalidFingerprint means fingerprint does not match format
	// (64 lower-hex). It duplicates CHECK `soul_seeds_fingerprint_format`.
	ErrSeedInvalidFingerprint = errors.New("soulseed: fingerprint format invalid (must be 64 lower-hex chars)")
)

// SoulSeed is runtime representation of a `soul_seeds` registry row.
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

// FingerprintFromCert computes SHA-256 fingerprint of certificate public key
// (DER form of SubjectPublicKeyInfo).
//
// Symmetric with `openssl x509 -pubkey | openssl dgst -sha256`: fingerprint is
// bound to key, not to the full DER certificate (the latter changes when the
// same key is reissued).
func FingerprintFromCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ValidFingerprintFormat checks format (64 lower-hex). Caller validates before
// round trip; PG CHECK guards DB side.
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
