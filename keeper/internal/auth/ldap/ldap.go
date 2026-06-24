// Package ldap — LDAP-аутентификация операторов (Archon) через search-bind +
// group-search. ADR-058(c) (LDAP-часть принята).
//
// Flow (ADR-058(c)):
//  1. POST /auth/ldap/login {username, password} поверх HTTPS.
//  2. connect по LDAPS или StartTLS (plaintext-LDAP запрещён конфигом).
//  3. search-bind: service-account ищет user-DN по user_filter → re-bind
//     этим DN + введённым паролем (проверка пароля).
//  4. group-search (group_filter) → []groups.
//  5. вернуть auth.ExternalIdentity (Subject=user-DN, AID=derived из aid_attr,
//     Groups=...) — маппинг на роли делает auth.Mapper выше по стеку.
//
// Безопасность (ADR-058(g), «безопасность на первом месте»):
//   - TLS обязателен (ldaps:// либо StartTLS), иначе New отвергает конфиг;
//   - username экранируется ldap.EscapeFilter перед подстановкой в фильтр
//     (anti-injection);
//   - любая причина отказа (bad bind, не та запись, IdP-ошибка) санитизируется
//     в auth.ErrAuthFailed — наружу не утекает (anti-oracle); детали — только
//     debug-лог без пароля/bind-creds.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// BindMode — режим LDAP-bind (ADR-058(c), развилка №7). MVP — только search.
type BindMode string

const (
	// BindModeSearch — service-account ищет user-DN, затем re-bind паролем
	// пользователя. Единственный поддерживаемый режим стадии 1.
	BindModeSearch BindMode = "search"
)

// defaultAIDAttr — атрибут LDAP, из которого выводится AID, если aid_attr не
// задан (ADR-058: дефолт `uid`). uid выбран дефолтом, а не mail, потому что он
// короче, стабильнее (mail может меняться/переназначаться) и почти всегда
// присутствует в схеме person/inetOrgPerson.
const defaultAIDAttr = "uid"

// defaultGroupAttr — атрибут группы → имя для role-map по умолчанию.
const defaultGroupAttr = "cn"

// defaultTimeout — таймаут connect/bind/search по умолчанию.
const defaultTimeout = 10 * time.Second

// Config — резолвнутая конфигурация LDAP-аутентификатора. Секреты
// (BindPassword) уже резолвнуты из Vault на load-time (ADR-058(e)), сюда
// приходят как plaintext-значения, НЕ как *_ref. TLSCA — резолвнутый CA-bundle.
type Config struct {
	URL                string   // ldaps://host:636 | ldap://host:389 (последний только StartTLS)
	StartTLS           bool     // StartTLS поверх ldap:// (взаимоискл. с ldaps://)
	TLSCA              []byte   // резолвнутый CA-bundle для LDAPS (опц.)
	InsecureSkipVerify bool     // dev-only (semantic-WARN)
	BindMode           BindMode // search (MVP)
	BindDN             string   // service-account DN (search-режим)
	BindPassword       string   // резолвнут из bind_password_ref (search-режим)
	BaseDN             string   // корень поиска
	UserFilter         string   // (uid=%s) — %s = escaped username
	GroupFilter        string   // (member=%s) — %s = escaped user-DN
	GroupAttr          string   // атрибут группы → имя для role-map (дефолт cn)
	AIDAttr            string   // атрибут → AID (дефолт uid)
	TimeoutSeconds     int      // таймаут connect/bind/search (дефолт 10s)
}

// conn — узкое подмножество *ldapv3.Conn, нужное Authenticate. Интерфейс
// позволяет unit-тестам подменять соединение fake-conn-ом без реального LDAP.
type conn interface {
	Bind(username, password string) error
	Search(req *ldapv3.SearchRequest) (*ldapv3.SearchResult, error)
	Close() error
}

// dialFunc — фабрика соединения. По умолчанию [dialLDAP] (реальный go-ldap);
// тесты подменяют на fake. Возвращает уже TLS-защищённое соединение
// (ldaps:// — TLS на dial; ldap://+StartTLS — StartTLS поднят внутри).
type dialFunc func(ctx context.Context, cfg Config, tlsCfg *tls.Config) (conn, error)

// Authenticator выполняет LDAP search-bind-аутентификацию (ADR-058(c)).
type Authenticator struct {
	cfg    Config
	tlsCfg *tls.Config
	dial   dialFunc
	logger *slog.Logger
}

// New конструирует LDAP-аутентификатор из резолвнутого конфига.
//
// Валидирует инварианты безопасности (TLS-required, ldaps-vs-StartTLS,
// search ⇒ BindDN+BindPassword) — defense-in-depth поверх semantic-валидации
// config-слоя (config может быть собран программно, минуя YAML-валидацию).
func New(cfg Config, logger *slog.Logger) (*Authenticator, error) {
	isLDAPS := strings.HasPrefix(cfg.URL, "ldaps://")
	isPlainLDAP := strings.HasPrefix(cfg.URL, "ldap://")

	if !isLDAPS && !(isPlainLDAP && cfg.StartTLS) {
		return nil, fmt.Errorf("auth/ldap: plaintext LDAP forbidden: url %q must be ldaps:// or ldap:// with start_tls", cfg.URL)
	}
	if isLDAPS && cfg.StartTLS {
		return nil, errors.New("auth/ldap: ldaps:// and start_tls are mutually exclusive")
	}
	mode := cfg.BindMode
	if mode == "" {
		mode = BindModeSearch
	}
	if mode != BindModeSearch {
		return nil, fmt.Errorf("auth/ldap: unsupported bind_mode %q (only %q in stage 1)", cfg.BindMode, BindModeSearch)
	}
	if cfg.BindDN == "" || cfg.BindPassword == "" {
		return nil, errors.New("auth/ldap: bind_mode=search requires bind_dn and bind_password")
	}
	if cfg.BaseDN == "" {
		return nil, errors.New("auth/ldap: base_dn is required")
	}
	if cfg.UserFilter == "" {
		return nil, errors.New("auth/ldap: user_filter is required")
	}

	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}

	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{cfg: cfg, tlsCfg: tlsCfg, dial: dialLDAP, logger: logger}, nil
}

// buildTLSConfig собирает *tls.Config: ServerName из host URL-а, RootCAs из
// TLSCA (если задан), InsecureSkipVerify (dev-only).
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	t := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-out оператора (dev-only, semantic-WARN); default false
		ServerName:         hostFromURL(cfg.URL),
	}
	if len(cfg.TLSCA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.TLSCA) {
			return nil, errors.New("auth/ldap: tls.ca_ref does not contain a valid PEM certificate")
		}
		t.RootCAs = pool
	}
	return t, nil
}

// hostFromURL извлекает host (без схемы и порта) для tls.ServerName.
func hostFromURL(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "ldaps://"), "ldap://")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// Method реализует auth.Authenticator: возвращает "ldap" (ADR-058(a)).
func (a *Authenticator) Method() string { return "ldap" }

// Authenticate выполняет search-bind + group-search и возвращает внешнюю
// identity. Любая ошибка наружу — auth.ErrAuthFailed (anti-oracle); детали —
// только debug-лог без пароля/bind-creds.
func (a *Authenticator) Authenticate(ctx context.Context, username, password string) (auth.ExternalIdentity, error) {
	// Пустой пароль — частый источник «unauthenticated bind» (RFC 4513 §5.1.2):
	// некоторые серверы трактуют bind с пустым паролем как анонимный успех.
	// Отбиваем явно.
	if username == "" || password == "" {
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	c, err := a.dial(ctx, a.cfg, a.tlsCfg)
	if err != nil {
		a.debugFail("dial", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	defer func() { _ = c.Close() }()

	// 1. bind service-account.
	if err := c.Bind(a.cfg.BindDN, a.cfg.BindPassword); err != nil {
		a.debugFail("service bind", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 2. search user (escaped username — anti-injection).
	aidAttr := a.aidAttr()
	userReq := &ldapv3.SearchRequest{
		BaseDN:       a.cfg.BaseDN,
		Scope:        ldapv3.ScopeWholeSubtree,
		DerefAliases: ldapv3.NeverDerefAliases,
		SizeLimit:    2, // >1 → ambiguous, отбиваем
		TimeLimit:    a.timeoutSeconds(),
		Filter:       fmt.Sprintf(a.cfg.UserFilter, ldapv3.EscapeFilter(username)),
		Attributes:   []string{aidAttr},
	}
	res, err := c.Search(userReq)
	if err != nil {
		a.debugFail("user search", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	if len(res.Entries) != 1 {
		a.logger.Debug("auth/ldap: user search did not return exactly one entry",
			slog.Int("entries", len(res.Entries)))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}
	userDN := res.Entries[0].DN
	aid := strings.ToLower(res.Entries[0].GetAttributeValue(aidAttr))
	if aid == "" {
		a.logger.Debug("auth/ldap: aid attribute empty", slog.String("aid_attr", aidAttr))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 3. re-bind как user-DN + введённый password (проверка пароля).
	if err := c.Bind(userDN, password); err != nil {
		a.debugFail("user bind", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 4. group-search (опционально — если group_filter задан).
	groups, err := a.searchGroups(c, userDN)
	if err != nil {
		a.debugFail("group search", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	return auth.ExternalIdentity{
		Subject:  userDN,
		AID:      aid,
		Username: username,
		Groups:   groups,
	}, nil
}

// searchGroups ищет группы пользователя по group_filter (member=<userDN>) и
// собирает имена по GroupAttr. Пустой group_filter → нет групп (Mapper отвергнет
// по ErrNoRoleMapping).
func (a *Authenticator) searchGroups(c conn, userDN string) ([]string, error) {
	if a.cfg.GroupFilter == "" {
		return nil, nil
	}
	groupAttr := a.groupAttr()
	req := &ldapv3.SearchRequest{
		BaseDN:       a.cfg.BaseDN,
		Scope:        ldapv3.ScopeWholeSubtree,
		DerefAliases: ldapv3.NeverDerefAliases,
		TimeLimit:    a.timeoutSeconds(),
		Filter:       fmt.Sprintf(a.cfg.GroupFilter, ldapv3.EscapeFilter(userDN)),
		Attributes:   []string{groupAttr},
	}
	res, err := c.Search(req)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if v := e.GetAttributeValue(groupAttr); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

func (a *Authenticator) aidAttr() string {
	if a.cfg.AIDAttr != "" {
		return a.cfg.AIDAttr
	}
	return defaultAIDAttr
}

func (a *Authenticator) groupAttr() string {
	if a.cfg.GroupAttr != "" {
		return a.cfg.GroupAttr
	}
	return defaultGroupAttr
}

func (a *Authenticator) timeoutSeconds() int {
	if a.cfg.TimeoutSeconds > 0 {
		return a.cfg.TimeoutSeconds
	}
	return int(defaultTimeout / time.Second)
}

// debugFail логирует причину отказа на debug-уровне. НИКОГДА не логирует пароль
// или bind-creds — только тег этапа и текст ошибки (go-ldap не кладёт пароли в
// текст ошибки bind-а).
func (a *Authenticator) debugFail(stage string, err error) {
	a.logger.Debug("auth/ldap: authentication failed",
		slog.String("stage", stage), slog.Any("error", err))
}

// dialLDAP — реальная фабрика соединения. Для ldaps:// — TLS на dial; для
// ldap://+StartTLS — plain dial + StartTLS. Таймаут — из cfg.
func dialLDAP(_ context.Context, cfg Config, tlsCfg *tls.Config) (conn, error) {
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	var (
		c   *ldapv3.Conn
		err error
	)
	if strings.HasPrefix(cfg.URL, "ldaps://") {
		c, err = ldapv3.DialURL(cfg.URL, ldapv3.DialWithTLSConfig(tlsCfg))
	} else {
		c, err = ldapv3.DialURL(cfg.URL)
	}
	if err != nil {
		return nil, err
	}
	c.SetTimeout(timeout)

	if cfg.StartTLS {
		if err := c.StartTLS(tlsCfg); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("start_tls: %w", err)
		}
	}
	return c, nil
}

// compile-time assertion: *Authenticator реализует auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)

// compile-time assertion: *ldapv3.Conn удовлетворяет узкому conn-интерфейсу.
var _ conn = (*ldapv3.Conn)(nil)
