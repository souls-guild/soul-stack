// Package auth — федеративная аутентификация операторов (Archon) поверх
// внешних identity-провайдеров (LDAP / OAuth2-OIDC). ADR-058 (СТАТУС: draft).
//
// ★ СКЕЛЕТ. Это каркас под ADR-058 (propose-and-wait): интерфейсы +
// типы данных + TODO-заглушки. Реальная LDAP/OIDC-логика и интеграция в
// прод-auth-flow добавляются ТОЛЬКО после одобрения дизайна пользователем.
// Сейчас здесь нет ни сетевых вызовов, ни импортов go-oidc/go-ldap/oauth2.
//
// Модель (ADR-058): внешний IdP аутентифицирует человека-Архонта, Keeper
// ВАЛИДИРУЕТ результат, МАППИТ внешнюю identity на реестр operators(aid) +
// RBAC-роли и выпускает ВНУТРЕННИЙ JWT существующим jwt.Issuer (ADR-014).
// Вся остальная система (auth-middleware, RBAC, MCP, OpenAPI) остаётся
// JWT-based и НЕ меняется.
package auth

import (
	"context"
	"errors"
)

// ExternalIdentity — результат успешной внешней аутентификации (LDAP-bind
// либо OIDC id_token), ещё ДО маппинга на operators(aid). Чистый снимок того,
// что отдал внешний IdP, без проектных решений (AID/роли назначает Mapper).
//
// Subject — стабильный идентификатор у IdP (OIDC `sub` либо LDAP user-DN).
// AID — derived проектный идентификатор оператора (operators.aid), выведенный
// Authenticator-ом из сконфигурированного атрибута/claim (LDAP `aid_attr`,
// дефолт `uid`; OIDC `aid_claim`). Отделён от Subject, потому что Subject —
// сырой идентификатор IdP (user-DN), а AID — это то, под чем оператор живёт в
// реестре и в JWT.sub. Mapper берёт AID именно отсюда.
// Email / Username — опц. человеко-читаемые поля.
// Groups — членство во внешних группах (источник role-mapping).
// Claims — сырые дополнительные claims/атрибуты (для расширяемого маппинга).
type ExternalIdentity struct {
	Subject  string
	AID      string
	Email    string
	Username string
	Groups   []string
	Claims   map[string]any
}

// MappedOperator — внешняя identity, отображённая на проектный субъект
// авторизации: AID реестра operators + RBAC-роли. Это то, из чего jwt.Issuer
// выпускает внутренний токен (claims sub=AID, roles=Roles по ADR-014).
//
// Provisioned=true означает, что Mapper создал НОВУЮ строку operators
// (auto-provision, развилка ADR-058(g) №1); false — оператор уже существовал.
type MappedOperator struct {
	AID         string
	Roles       []string
	Provisioned bool
}

// Authenticator — общий контракт способа федеративной аутентификации.
// Реализации: ldap.Authenticator (bind) и oidc.Authenticator (code-flow).
// Возвращает ТОЛЬКО факт «кто это у внешнего IdP» — маппинг на AID и выпуск
// JWT делаются выше по стеку (Mapper + jwt.Issuer), чтобы способ аутентификации
// не знал о реестре operators и RBAC.
type Authenticator interface {
	// Method — значение auth_method (operator.AuthMethod) для аудита/строки
	// operators: "ldap" | "oidc" (ADR-058(a)).
	Method() string
}

// Mapper отображает внешнюю identity на operators(aid) + роли (ADR-058(d)).
// Инкапсулирует развилки provisioning (auto-provision vs pre-register, №1)
// и источника ролей (внешние группы vs реестр, №2) — обе ждут решения
// пользователя, поэтому здесь только контракт.
type Mapper interface {
	// Map преобразует ExternalIdentity в MappedOperator либо возвращает ошибку
	// (оператор revoked / не найден при pre-register / нет роли-маппинга и т.п.).
	Map(ctx context.Context, ext ExternalIdentity) (MappedOperator, error)
}

// Сентинел-ошибки федеративной аутентификации. Публичный HTTP-detail для них
// классифицируется отдельно (как jwt.ClassifyVerifyErr, ADR-014): наружу не
// должны утекать причины (anti-oracle), это уточняется на этапе имплементации.
var (
	// ErrAuthFailed — внешняя аутентификация не прошла (bad credentials,
	// невалидный id_token, IdP отверг).
	ErrAuthFailed = errors.New("auth: external authentication failed")
	// ErrOperatorRevoked — внешняя identity маппится на revoked-оператора;
	// federated-login revoked-оператора запрещён (ADR-058(d) revocation-инвариант).
	ErrOperatorRevoked = errors.New("auth: operator revoked")
	// ErrOperatorNotProvisioned — pre-register-режим, оператор заранее не заведён.
	ErrOperatorNotProvisioned = errors.New("auth: operator not pre-registered")
	// ErrNoRoleMapping — у внешней identity нет ни одной маппящейся роли.
	ErrNoRoleMapping = errors.New("auth: no role mapping for external identity")
)
