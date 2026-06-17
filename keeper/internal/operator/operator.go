// Package operator — типы реестра Архонтов (operators) под ADR-014.
//
// M0.5a: только Go-struct + helper-методы + AID-regex для применения вне
// SQL CHECK. CRUD (Insert / SelectByAID / Revoke / List) и self-lockout
// инвариант (ADR-013 + rbac.md) добавляются в M0.5c вместе с bootstrap-логикой
// и Operator API в M0.6.
package operator

import (
	"regexp"
	"time"
)

// AuthMethod — форма credential Архонта.
//
// MVP по ADR-014: только AuthMethodJWT. AuthMethodMTLS и
// AuthMethodCombined зарезервированы как enum-расширения post-MVP
// (через `auth_method` колонку, без breaking changes схемы).
type AuthMethod string

const (
	AuthMethodJWT      AuthMethod = "jwt"
	AuthMethodMTLS     AuthMethod = "mtls"
	AuthMethodCombined AuthMethod = "combined"
)

// AIDPattern — форма Archon ID (ADR-014 amendment 2026-05-29): первый
// символ — строчная ASCII-буква или цифра, далее 1..127 символов из
// `[a-z0-9._@-]`. Суммарная длина AID — 2..128 символов. Префикс
// `archon-` больше не обязателен.
//
// Charset намеренно узкий и безопасный: нет `/`/`\` (path-traversal),
// только ASCII-lowercase (нет unicode-двойников и регистра), нет
// управляющих/кавычек (нет инъекций). `@` и `.` разрешены для
// email-подобных внешних имён (LDAP/Keycloak auto-provision).
//
// Дублирует SQL CHECK `aid_format` (миграция 058) — нужно для прикладной
// валидации на стороне API-handler-ов до того, как запрос дойдёт до БД
// (better error messages, нет лишнего round-trip).
const AIDPattern = `^[a-z0-9][a-z0-9._@-]{1,127}$`

var aidRe = regexp.MustCompile(AIDPattern)

// ValidAID проверяет соответствие AID канонической форме.
func ValidAID(aid string) bool { return aidRe.MatchString(aid) }

// Operator — runtime-представление строки реестра operators
// (ADR-014, docs/keeper/storage.md → таблица operators).
//
// JSON-теги — для будущего Operator API (M0.6). NULL-семантика SQL
// маппится в указатели: CreatedByAID = nil у первого bootstrap-Archon-а,
// RevokedAt = nil у активного.
type Operator struct {
	AID          string         `json:"aid"`
	DisplayName  string         `json:"display_name"`
	AuthMethod   AuthMethod     `json:"auth_method"`
	CreatedAt    time.Time      `json:"created_at"`
	CreatedByAID *string        `json:"created_by_aid,omitempty"`
	RevokedAt    *time.Time     `json:"revoked_at,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// IsRevoked — у Operator установлен RevokedAt (non-nil). Активные JWT
// существующих сессий по ADR-014(d) продолжают работать до `exp`;
// проверка IsRevoked нужна на write-path-операциях и для UI.
func (o *Operator) IsRevoked() bool { return o.RevokedAt != nil }

// IsBootstrap — Operator создан через `keeper init` (CreatedByAID = nil).
// Полезно для audit / RBAC-проверок «нельзя удалить bootstrap-Archon-а,
// если он последний с *-permission» (self-lockout, ADR-014 + rbac.md).
func (o *Operator) IsBootstrap() bool { return o.CreatedByAID == nil }
