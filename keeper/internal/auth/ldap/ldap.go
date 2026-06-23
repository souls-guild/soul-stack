// Package ldap — LDAP-аутентификация операторов (Archon) через bind +
// group-search. ADR-058(c) (СТАТУС: draft).
//
// ★ СКЕЛЕТ. Каркас под ADR-058: интерфейс Authenticator + конфиг-зависимый
// конструктор + TODO-заглушка реального bind-flow. Реальная LDAP-логика
// (LDAPS/StartTLS-connect, search-bind / direct-bind, group-search) НЕ
// реализована — добавляется после одобрения дизайна. Здесь НЕТ импорта
// github.com/go-ldap/ldap/v3 (зависимость добавляется на этапе имплементации).
//
// Flow (ADR-058(c), после имплементации):
//  1. POST /auth/ldap/login {username, password} поверх HTTPS.
//  2. connect по LDAPS или StartTLS (plaintext-LDAP запрещён конфигом).
//  3. search-bind: service-account ищет user-DN по user_filter → re-bind
//     этим DN + введённым паролем; ИЛИ direct-bind по user_dn_template.
//  4. group-search (group_filter) → []groups.
//  5. вернуть auth.ExternalIdentity (Subject=user-DN, Groups=...) — маппинг
//     на AID+роли делает auth.Mapper выше по стеку.
package ldap

import (
	"context"
	"errors"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// BindMode — режим LDAP-bind (ADR-058(c), развилка №7).
type BindMode string

const (
	// BindModeSearch — service-account ищет user-DN, затем re-bind паролем
	// пользователя. Рекомендуется (гибкие схемы каталога).
	BindModeSearch BindMode = "search"
	// BindModeDirect — DN строится по шаблону, bind сразу. Проще, но ломается
	// на нетривиальных DN.
	BindModeDirect BindMode = "direct"
)

// Config — резолвнутая конфигурация LDAP-аутентификатора. Секреты
// (BindPassword) уже резолвнуты из Vault на load-time (ADR-058(e)), сюда
// приходят как plaintext-значения, НЕ как *_ref. TLSCA — резолвнутый CA-bundle.
//
// Поля 1:1 с config.KeeperAuthLDAP (shared/config/keeper.go), но без *_ref:
// config-слой держит ссылки, этот слой — резолвнутые значения.
type Config struct {
	URL                string   // ldaps://host:636 | ldap://host:389 (последний только StartTLS)
	StartTLS           bool     // StartTLS поверх ldap:// (взаимоискл. с ldaps://)
	TLSCA              []byte   // резолвнутый CA-bundle для LDAPS (опц.)
	InsecureSkipVerify bool     // dev-only (semantic-WARN)
	BindMode           BindMode // search | direct
	BindDN             string   // service-account DN (search-режим)
	BindPassword       string   // резолвнут из bind_password_ref (search-режим)
	BaseDN             string   // корень поиска
	UserFilter         string   // (uid=%s) — %s = username (search-режим)
	UserDNTemplate     string   // uid=%s,ou=people,... (direct-режим)
	GroupFilter        string   // (member=%s) — %s = user-DN
	GroupAttr          string   // атрибут группы → имя для role-map (напр. cn)
	AIDAttr            string   // атрибут → AID (напр. uid | mail)
	TimeoutSeconds     int      // таймаут connect/bind/search
}

// Authenticator выполняет LDAP-аутентификацию (ADR-058(c)).
//
// ★ Реальный LDAP-клиент (go-ldap/v3) и пул соединений — поля добавляются
// на этапе имплементации. Сейчас держит только резолвнутый конфиг.
type Authenticator struct {
	cfg Config
	// TODO(ADR-058 impl): резолвнутый *tls.Config; фабрика *ldap.Conn;
	// (опц.) пул соединений для service-account search-bind.
}

// New конструирует LDAP-аутентификатор из резолвнутого конфига.
//
// ★ СКЕЛЕТ: валидация инвариантов (ldaps://-vs-StartTLS, search требует
// BindDN+BindPassword, обязательность TLS) — TODO на этапе имплементации.
func New(cfg Config) (*Authenticator, error) {
	// TODO(ADR-058 impl): провалидировать TLS-required (ADR-058(g));
	// собрать *tls.Config из TLSCA/InsecureSkipVerify; проверить
	// взаимоисключимость ldaps:// и StartTLS; для BindModeSearch — наличие
	// BindDN+BindPassword.
	return &Authenticator{cfg: cfg}, nil
}

// Method реализует auth.Authenticator: возвращает "ldap" (ADR-058(a)).
func (a *Authenticator) Method() string { return "ldap" }

// Authenticate выполняет bind+group-search для (username, password) и
// возвращает внешнюю identity. Маппинг на AID+роли — auth.Mapper выше.
//
// ★ СКЕЛЕТ-ЗАГЛУШКА: возвращает errNotImplemented. Реальный flow (connect →
// bind → group-search) добавляется после одобрения ADR-058.
func (a *Authenticator) Authenticate(_ context.Context, username, password string) (auth.ExternalIdentity, error) {
	_ = username
	_ = password
	// TODO(ADR-058 impl): LDAPS/StartTLS connect; search-bind/direct-bind;
	// group-search; маппинг bad-bind → auth.ErrAuthFailed (без утечки причины);
	// таймауты по cfg.TimeoutSeconds.
	return auth.ExternalIdentity{}, errNotImplemented
}

// errNotImplemented — маркер скелета: метод объявлен, логики ещё нет (ADR-058
// в статусе draft). Обёрнут в auth.ErrAuthFailed, чтобы случайное раннее
// включение не превратилось в open-login.
var errNotImplemented = errors.New("auth/ldap: not implemented (ADR-058 draft)")

// compile-time assertion: *Authenticator реализует auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)
