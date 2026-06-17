// Package bootstraptoken — типы и CRUD реестра одноразовых SoulSeed-токенов
// (`bootstrap_tokens`) под docs/soul/onboarding.md.
//
// Plain-токен генерируется в [Generate] (256 бит crypto-random, base64url),
// возвращается оператору **один раз** и больше не существует в системе.
// В БД хранится только `token_hash` = SHA-256 (hex). При предъявлении в
// `Bootstrap`-RPC хеш считается заново и матчится с реестром (см. [Burn]).
//
// «Сжигание» (Burn) — UPDATE одной транзакцией с условием
// `used_at IS NULL AND expires_at > NOW()` (race-safe против одновременного
// предъявления одного токена дважды).
package bootstraptoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TokenByteLen — длина plain-токена до base64url-кодирования. 32 байта =
// 256 бит энтропии — достаточно, чтобы исключить guess-атаку даже без TTL.
// Канонический формат документирован в docs/soul/onboarding.md.
const TokenByteLen = 32

// HashHexLen — длина hex-представления SHA-256 (64 символа). Дублирует
// CHECK `bootstrap_tokens_token_hash_format` из 008.
const HashHexLen = 64

// DefaultTokenTTL — TTL bootstrap-токена по умолчанию (docs/soul/onboarding.md:
// «токен одноразовый, TTL по умолчанию 24h»). Используется Operator API при
// выписке через `POST /v1/souls` и `issue-token`, если оператор не задал
// иное (override-поле — post-MVP).
const DefaultTokenTTL = 24 * time.Hour

// PlainToken — sensitive-обёртка над plain-значением токена. Возвращается
// [Generate] / [Insert] один раз. String() сознательно НЕ реализован —
// случайный `fmt.Print(token)` или `slog.Any("token", token)` напечатает
// `{REDACTED}` через стандартный fmt-rendering непрозрачной struct-ы.
//
// Чтобы получить plain-значение для записи в файл/возврата клиенту,
// caller вызывает [PlainToken.Reveal]. Это намеренно явный шаг — grep по
// `.Reveal(` показывает все места, где plain попадает в I/O.
type PlainToken struct {
	v string
}

// Reveal возвращает plain-значение токена. ТОЛЬКО для записи в файл/возврата
// клиенту/тесты. НИКОГДА не логировать.
func (t PlainToken) Reveal() string { return t.v }

// Hash возвращает SHA-256 от plain-токена в hex. Используется и в [Insert]
// (при выписке), и в [Burn] (при предъявлении в Bootstrap-RPC).
func (t PlainToken) Hash() string {
	sum := sha256.Sum256([]byte(t.v))
	return hex.EncodeToString(sum[:])
}

// HashToken — хеш произвольной строки в SHA-256 hex. Используется
// gRPC-handler-ом `Bootstrap`, который получает plain из protobuf-поля и
// должен немедленно сравнить с реестром, не оборачивая в PlainToken.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Generate выпускает новый plain-токен (32 байта crypto-random → base64url
// без padding-а). Возвращает обёртку [PlainToken]; hex-хеш доступен через
// Hash().
//
// Используется только Operator API при выписке нового токена; в gRPC
// Bootstrap-handler-е plain-токен приходит от клиента, не генерится здесь.
func Generate() (PlainToken, error) {
	buf := make([]byte, TokenByteLen)
	if _, err := rand.Read(buf); err != nil {
		return PlainToken{}, fmt.Errorf("bootstraptoken: crypto/rand: %w", err)
	}
	// base64url без padding-а — компактный URL-safe формат, симметричный
	// JWT (golang-jwt тоже base64url-no-padding).
	return PlainToken{v: base64.RawURLEncoding.EncodeToString(buf)}, nil
}

// Record — runtime-представление строки `bootstrap_tokens`. Plain-значение
// токена в Record никогда не лежит — только хеш.
type Record struct {
	TokenID      string     `json:"token_id"`
	SID          string     `json:"sid"`
	TokenHash    string     `json:"-"` // sensitive: не сериализуем наружу.
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	UsedAt       *time.Time `json:"used_at,omitempty"`
	UsedByKID    *string    `json:"used_by_kid,omitempty"`
	CreatedByAID *string    `json:"created_by_aid,omitempty"`
}

// IsActive — токен не сожжён и не истёк (на момент проверки в Go;
// authoritative-чек — на стороне БД через WHERE-clause в [Burn]).
func (r *Record) IsActive(now time.Time) bool {
	return r.UsedAt == nil && r.ExpiresAt.After(now)
}

// ValidHashFormat проверяет, что строка — валидный SHA-256 hex (64 lower-hex).
// Дублирует CHECK `bootstrap_tokens_token_hash_format` для отказа до round-trip-а.
func ValidHashFormat(hash string) bool {
	if len(hash) != HashHexLen {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errInvalidHash — utility-error для validate-фазы CRUD-функций (не
// sentinel, не уходит наверх как `errors.Is`-цель).
var errInvalidHash = errors.New("bootstraptoken: token_hash format invalid (must be 64 lower-hex chars)")
