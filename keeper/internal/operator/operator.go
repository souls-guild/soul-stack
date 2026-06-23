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

	// ★ СКЕЛЕТ под ADR-058 (СТАТУС: draft) — федеративная аутентификация.
	// Only-add расширение enum (как mTLS/combined post-MVP в ADR-014):
	// фиксирует, каким внешним способом оператор пришёл. Сам внутренний JWT
	// после выпуска одинаков. SQL CHECK `auth_method IN (...)` расширяется
	// отдельной only-add миграцией ТОЛЬКО после одобрения ADR-058.
	AuthMethodLDAP AuthMethod = "ldap" // ADR-058 draft
	AuthMethodOIDC AuthMethod = "oidc" // ADR-058 draft
)

// CreatedVia — источник заведения оператора (ADR-058(d)). Отличается от
// [AuthMethod]: auth_method отвечает «чем оператор логинится», created_via —
// «откуда он вообще появился в реестре». Bootstrap-Архонт заведён через
// `keeper init` (created_via=bootstrap), но логинится по jwt; federated-оператор
// заведён auto-provision-ом (created_via=ldap/oidc); `archon-system` —
// system-якорь для FK-атрибуции system-инициированных вставок.
//
// Тип — string-alias (а не отдельный enum-тип), потому что значение хранится в
// общем поле Operator.CreatedVia рядом с другими строковыми колонками и не
// требует методов; домен валидируется в [Insert] и SQL CHECK `created_via_valid`
// (миграция 084).
type CreatedVia = string

const (
	CreatedViaBootstrap CreatedVia = "bootstrap"
	CreatedViaUser      CreatedVia = "user"
	CreatedViaLDAP      CreatedVia = "ldap"
	CreatedViaOIDC      CreatedVia = "oidc"
	CreatedViaSystem    CreatedVia = "system"
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
	CreatedVia   CreatedVia     `json:"created_via"`
	RevokedAt    *time.Time     `json:"revoked_at,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// IsRevoked — у Operator установлен RevokedAt (non-nil). Активные JWT
// существующих сессий по ADR-014(d) продолжают работать до `exp`;
// проверка IsRevoked нужна на write-path-операциях и для UI.
func (o *Operator) IsRevoked() bool { return o.RevokedAt != nil }

// IsBootstrap — Operator создан через `keeper init` (created_via='bootstrap').
// Полезно для audit / RBAC-проверок «нельзя удалить bootstrap-Archon-а,
// если он последний с *-permission» (self-lockout, ADR-014 + rbac.md).
//
// ADR-058(d): признак перенесён с `CreatedByAID == nil` на
// `CreatedVia == CreatedViaBootstrap`. После легализации NULL у created_by_aid
// для не-bootstrap-строк (archon-system, federated-операторы) проверка по
// created_by_aid дала бы ложноположительный bootstrap-флаг — единственный
// авторитет «это первый Архонт» теперь created_via.
func (o *Operator) IsBootstrap() bool { return o.CreatedVia == CreatedViaBootstrap }
