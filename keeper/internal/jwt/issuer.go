// Package jwt — JWT issuer for Archon operators.
//
// MVP (ADR-014): HS256 (HMAC-SHA256), signing key from Vault KV
// `secret/keeper/jwt-signing-key`. Claims: `iss`/`sub`/`iat`/`exp`/`roles` +
// `bootstrap_initial` (only for the first Archon per ADR-013).
//
// The verify side isn't implemented here — that's the Operator API
// middleware (M0.6). Post-MVP: RS256 / ED25519 + signing key via Vault transit.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// minSigningKeyBytes — minimum HMAC key length for HS256. RFC 7518 §3.2:
// "A key of the same size as the hash output … or larger MUST be used".
// SHA-256 → 32 bytes.
const minSigningKeyBytes = 32

// archonClaims — jwtv5.RegisteredClaims extended with project fields.
//
// `roles` — array of RBAC role names (cluster-admin / read-only / ...).
// `bootstrap_initial` — true only for the first Archon (ADR-013).
type archonClaims struct {
	Roles            []string `json:"roles"`
	BootstrapInitial bool     `json:"bootstrap_initial,omitempty"`
	jwtv5.RegisteredClaims
}

// Issuer issues JWTs for Archon operators.
//
// Fields are private; the constructor validates key length.
type Issuer struct {
	signingKey []byte
	issuer     string
}

// NewIssuer creates an issuer. signingKey must be >= 32 bytes (HS256
// requires a key no shorter than the hash).
//
// issuer — value for the `iss` claim (e.g. "keeper.example.com" from
// `keeper.yml::auth.jwt.issuer`).
func NewIssuer(signingKey []byte, issuer string) (*Issuer, error) {
	if len(signingKey) < minSigningKeyBytes {
		return nil, fmt.Errorf("jwt: signing key length %d < %d (HS256 minimum)", len(signingKey), minSigningKeyBytes)
	}
	if issuer == "" {
		return nil, errors.New("jwt: issuer is empty")
	}
	return &Issuer{signingKey: signingKey, issuer: issuer}, nil
}

// Issue issues an HS256-signed JWT for an archon.
//
// aid — Archon ID (e.g. "archon-alice"), goes into the `sub` claim.
// roles — array of RBAC roles, `roles` claim.
// ttl — token lifetime, drives `exp` (now + ttl).
// bootstrapInitial — true only for the first Archon (ADR-013, `bootstrap_initial`
// claim; omitted for all other operators).
func (i *Issuer) Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error) {
	if aid == "" {
		return "", errors.New("jwt: aid is empty")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("jwt: ttl must be positive, got %s", ttl)
	}

	now := time.Now().UTC()
	claims := archonClaims{
		Roles:            roles,
		BootstrapInitial: bootstrapInitial,
		RegisteredClaims: jwtv5.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   aid,
			IssuedAt:  jwtv5.NewNumericDate(now),
			ExpiresAt: jwtv5.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims)
	signed, err := tok.SignedString(i.signingKey)
	if err != nil {
		return "", fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, nil
}
