// Package jwt — issuer JWT-токенов для Archon-операторов.
//
// MVP (ADR-014): HS256 (HMAC-SHA256), signing key из Vault KV
// `secret/keeper/jwt-signing-key`. Claims: `iss`/`sub`/`iat`/`exp`/`roles` +
// `bootstrap_initial` (только у первого Архонта по ADR-013).
//
// Verify-сторона не реализуется здесь — это middleware Operator API (M0.6).
// Post-MVP: RS256 / ED25519 + signing key через Vault transit.
package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
)

// minSigningKeyBytes — минимальная длина HMAC ключа для HS256. RFC 7518 §3.2:
// «A key of the same size as the hash output … or larger MUST be used».
// SHA-256 → 32 байта.
const minSigningKeyBytes = 32

// archonClaims — расширение jwtv5.RegisteredClaims с проектными полями.
//
// `roles` — массив имён RBAC-ролей (cluster-admin / read-only / ...).
// `bootstrap_initial` — true только у первого Архонта (ADR-013).
type archonClaims struct {
	Roles            []string `json:"roles"`
	BootstrapInitial bool     `json:"bootstrap_initial,omitempty"`
	jwtv5.RegisteredClaims
}

// Issuer выпускает JWT-токены для Archon-операторов.
//
// Поля приватные; конструктор валидирует длину ключа.
type Issuer struct {
	signingKey []byte
	issuer     string
}

// NewIssuer создаёт issuer. signingKey должен быть >= 32 байт (HS256
// требует ключ не короче хэша).
//
// issuer — значение для claim `iss` (например, "keeper.example.com" из
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

// Issue выпускает HS256-подписанный JWT для archon-а.
//
// aid — Archon ID (e.g. "archon-alice"), идёт в claim `sub`.
// roles — массив RBAC-ролей, claim `roles`.
// ttl — время жизни токена, влияет на `exp` (now + ttl).
// bootstrapInitial — true только у первого Архонта (ADR-013, claim
// `bootstrap_initial`; для остальных операторов поле omit-ится).
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
