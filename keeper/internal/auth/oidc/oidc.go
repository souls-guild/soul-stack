// Package oidc — OAuth2/OIDC-аутентификация операторов (Archon) через
// authorization-code flow. ADR-058(b) (СТАТУС: draft).
//
// ★ СКЕЛЕТ. Каркас под ADR-058: интерфейс + конфиг + TODO-заглушки login/
// callback. Реальный OIDC-flow (discovery, JWKS-verify id_token, code-exchange,
// state/nonce/PKCE) НЕ реализован — после одобрения дизайна. Здесь НЕТ импорта
// github.com/coreos/go-oidc/v3 и golang.org/x/oauth2 (добавляются на импле).
//
// Flow (ADR-058(b), после имплементации):
//  1. GET /auth/oidc/login → сгенерировать state+nonce+PKCE, сохранить в
//     короткоживущем server-side store (Redis TTL ~5m, ADR-006), redirect на IdP.
//  2. человек аутентифицируется у IdP.
//  3. GET /auth/oidc/callback?code&state → проверить state (CSRF), обменять
//     code на токены (PKCE code_verifier), ВАЛИДИРОВАТЬ id_token (JWKS-подпись,
//     iss, aud==client_id, exp/iat, nonce-match), извлечь sub/email/groups.
//  4. вернуть auth.ExternalIdentity — маппинг на AID+роли делает auth.Mapper.
package oidc

import (
	"context"
	"errors"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// Config — резолвнутая конфигурация OIDC-аутентификатора. ClientSecret уже
// резолвнут из Vault на load-time (ADR-058(e)) — сюда приходит plaintext, НЕ
// *_ref. TLSCA — резолвнутый CA-bundle IdP (опц.).
//
// Поля 1:1 с config.KeeperAuthOIDC (shared/config/keeper.go), но без *_ref.
type Config struct {
	Issuer       string   // https://idp/realms/... (discovery base)
	ClientID     string   // OAuth2 client_id
	ClientSecret string   // резолвнут из client_secret_ref
	RedirectURL  string   // https://keeper/auth/oidc/callback
	Scopes       []string // openid, email, profile, groups
	TLSCA        []byte   // резолвнутый CA-bundle IdP (опц.)
	AIDClaim     string   // claim → AID (sub | email | preferred_username)
	GroupsClaim  string   // claim → []groups для role-map
	UsePKCE      bool     // PKCE (ADR-058 развилка №6; рекомендуется true)
}

// Authorization — выдаётся при старте login-flow: куда редиректить браузер
// и какой непрозрачный state хранить server-side (Redis), чтобы сматчить на
// callback. CodeVerifier (PKCE) и Nonce ОСТАЮТСЯ на сервере (в store), НЕ в URL.
type Authorization struct {
	RedirectTo string // authorization_endpoint IdP с query (client_id/redirect/scope/state/code_challenge)
	State      string // CSRF-токен; ключ записи в server-side store
}

// Authenticator выполняет OIDC code-flow (ADR-058(b)).
//
// ★ Реальные *oidc.Provider / *oidc.IDTokenVerifier / *oauth2.Config и
// server-side state-store — поля добавляются на этапе имплементации. Сейчас
// держит только резолвнутый конфиг.
type Authenticator struct {
	cfg Config
	// TODO(ADR-058 impl): провайдер из go-oidc discovery; verifier (JWKS, aud);
	// *oauth2.Config; server-side state/nonce/PKCE-store (Redis-backed).
}

// New конструирует OIDC-аутентификатор из резолвнутого конфига.
//
// ★ СКЕЛЕТ: discovery (`/.well-known/openid-configuration`), построение
// verifier и oauth2.Config — TODO на импле (требуют сетевого вызова к IdP).
func New(_ context.Context, cfg Config) (*Authenticator, error) {
	// TODO(ADR-058 impl): oidc.NewProvider(ctx, cfg.Issuer) с TLS из TLSCA;
	// provider.Verifier({ClientID, ...}); собрать oauth2.Config (endpoints из
	// provider, RedirectURL, Scopes); инициализировать state-store.
	return &Authenticator{cfg: cfg}, nil
}

// Method реализует auth.Authenticator: возвращает "oidc" (ADR-058(a)).
func (a *Authenticator) Method() string { return "oidc" }

// BeginLogin стартует authorization-code flow: генерирует state/nonce/PKCE,
// сохраняет их server-side и возвращает Authorization для redirect.
//
// ★ СКЕЛЕТ-ЗАГЛУШКА: errNotImplemented.
func (a *Authenticator) BeginLogin(_ context.Context) (Authorization, error) {
	// TODO(ADR-058 impl): crypto/rand state+nonce+code_verifier; S256
	// code_challenge; persist {state→(nonce,code_verifier,expiry)} в Redis;
	// собрать RedirectTo через oauth2.Config.AuthCodeURL(state, nonce, PKCE).
	return Authorization{}, errNotImplemented
}

// CompleteLogin завершает flow по (code, state): валидирует state, обменивает
// code, ВАЛИДИРУЕТ id_token (JWKS/iss/aud/exp/nonce) и возвращает внешнюю
// identity. Маппинг на AID+роли — auth.Mapper выше.
//
// ★ СКЕЛЕТ-ЗАГЛУШКА: errNotImplemented.
func (a *Authenticator) CompleteLogin(_ context.Context, code, state string) (auth.ExternalIdentity, error) {
	_ = code
	_ = state
	// TODO(ADR-058 impl): lookup+consume state из store (CSRF + одноразовость);
	// oauth2 Exchange(code, code_verifier); verifier.Verify(id_token); сверить
	// nonce; извлечь AIDClaim/GroupsClaim → auth.ExternalIdentity; невалидный
	// токен → auth.ErrAuthFailed (без утечки причины).
	return auth.ExternalIdentity{}, errNotImplemented
}

// errNotImplemented — маркер скелета (ADR-058 draft). Обёрнут в смысл
// auth.ErrAuthFailed, чтобы раннее включение не дало open-login.
var errNotImplemented = errors.New("auth/oidc: not implemented (ADR-058 draft)")

// compile-time assertion: *Authenticator реализует auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)
