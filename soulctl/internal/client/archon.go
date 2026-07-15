package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ArchonAPI holds the methods behind soulctl's archon commands. The MVP
// openapi has no whoami endpoint (see soulctl/README.md), so it's
// implemented client-side by decoding JWT claims + pinging any authorized
// endpoint.
type ArchonAPI struct {
	c *Client
}

// Ping checks the JWT via a lightweight list endpoint. Returns an APIError
// on 401/403, or a meaningful transport error on network failure.
//
// Uses `GET /v1/incarnations?limit=1` (minimal page, permission
// incarnation.list; cluster-admin always has it, an operator without the
// permission gets a 403 and finds out immediately).
func (a *ArchonAPI) Ping(ctx context.Context) error {
	return a.c.Do(ctx, "GET", "/v1/incarnations?limit=1", nil, nil)
}

// JWTClaims is the JWT claim set per ADR-014 / operator-api.md → § Auth.
type JWTClaims struct {
	Iss              string   `json:"iss,omitempty"`
	Sub              string   `json:"sub"` // AID
	Iat              int64    `json:"iat,omitempty"`
	Exp              int64    `json:"exp,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	BootstrapInitial bool     `json:"bootstrap_initial,omitempty"`
}

// DecodeJWTClaims parses claims from the JWT payload segment WITHOUT
// signature verification. This is fine for whoami: the signature was
// already verified by Keeper during `archon login` (ping returned 200) —
// soulctl just decodes already-accepted credentials.
//
// Returns a meaningful error on malformed input.
func DecodeJWTClaims(jwt string) (*JWTClaims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("JWT должен содержать три сегмента, получено %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some implementations add padding — try the std-base64 fallback.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("декодировать JWT payload: %w", err)
		}
	}
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("разобрать JWT payload: %w", err)
	}
	return &claims, nil
}

// Whoami combines Ping + DecodeJWTClaims. Returns the claims or an error.
func (a *ArchonAPI) Whoami(ctx context.Context) (*JWTClaims, error) {
	if err := a.c.Ping(ctx); err != nil {
		return nil, err
	}
	return DecodeJWTClaims(a.c.jwt)
}

// Ping is a public shortcut to Archon.Ping.
func (c *Client) Ping(ctx context.Context) error { return c.Archon.Ping(ctx) }
