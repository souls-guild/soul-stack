package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ArchonAPI — методы под archon-команды soulctl. Whoami-эндпоинта в openapi
// MVP нет (см. soulctl/README.md), поэтому он реализуется client-side через
// декодирование JWT claims + ping любым авторизованным endpoint-ом.
type ArchonAPI struct {
	c *Client
}

// Ping — проверка JWT через лёгкий list-эндпоинт. Возвращает APIError при
// 401/403, осмысленную ошибку транспорта при сети.
//
// Используется `GET /v1/incarnations?limit=1` (минимальная страница, permission
// incarnation.list; cluster-admin его всегда имеет, оператор без прав получит
// 403 и узнает об этом сразу).
func (a *ArchonAPI) Ping(ctx context.Context) error {
	return a.c.Do(ctx, "GET", "/v1/incarnations?limit=1", nil, nil)
}

// JWTClaims — клейм-набор JWT по ADR-014 / operator-api.md → § Auth.
type JWTClaims struct {
	Iss              string   `json:"iss,omitempty"`
	Sub              string   `json:"sub"` // AID
	Iat              int64    `json:"iat,omitempty"`
	Exp              int64    `json:"exp,omitempty"`
	Roles            []string `json:"roles,omitempty"`
	BootstrapInitial bool     `json:"bootstrap_initial,omitempty"`
}

// DecodeJWTClaims — парсит claims из payload части JWT БЕЗ верификации подписи.
// Это допустимо для whoami: подпись уже проверена Keeper-ом при `archon login`
// (ping вернул 200) — soulctl лишь декодирует уже принятые credentials.
//
// Возвращает осмысленную ошибку при битом формате.
func DecodeJWTClaims(jwt string) (*JWTClaims, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("JWT должен содержать три сегмента, получено %d", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Некоторые имплементации добавляют padding — попробуем std-base64 fallback.
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

// Whoami — комбинация Ping + DecodeJWTClaims. Возвращает claims или ошибку.
func (a *ArchonAPI) Whoami(ctx context.Context) (*JWTClaims, error) {
	if err := a.c.Ping(ctx); err != nil {
		return nil, err
	}
	return DecodeJWTClaims(a.c.jwt)
}

// Ping — публичный shortcut на Archon.Ping.
func (c *Client) Ping(ctx context.Context) error { return c.Archon.Ping(ctx) }
