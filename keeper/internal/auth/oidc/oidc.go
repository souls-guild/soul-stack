// Package oidc — OAuth2/OIDC-аутентификация операторов (Archon) через
// authorization-code flow с PKCE. ADR-058(b) (стадия 2, accepted + end-to-end).
//
// Модель (ADR-058): внешний IdP аутентифицирует человека-Архонта, Keeper
// ВАЛИДИРУЕТ id_token, извлекает claims (sub/groups) и возвращает
// auth.ExternalIdentity — маппинг на operators(aid)+роли и выпуск внутреннего
// JWT делает auth.Mapper + jwt.Issuer выше по стеку.
//
// Flow (ADR-058(b)):
//  1. BeginLogin (GET /auth/oidc/login) → crypto/rand state+nonce+PKCE-verifier;
//     S256 code_challenge; persist {state→(nonce,verifier)} в FlowStore (Redis
//     TTL ~5m, cluster-shared); redirect на authorization_endpoint IdP.
//  2. человек аутентифицируется у IdP.
//  3. CompleteLogin (GET /auth/oidc/callback?code&state) → Consume state из
//     store (CSRF + single-use), code-exchange с code_verifier (PKCE),
//     ВАЛИДАЦИЯ id_token через go-oidc verifier (JWKS-подпись / iss / aud==
//     client_id / exp / iat), сверка nonce (anti-replay), извлечение claims.
//
// Безопасность (ADR-058(g), «безопасность на первом месте»):
//   - PKCE обязателен (S256): code_verifier остаётся server-side, без него
//     code-exchange невозможен (anti code-interception);
//   - state — CSRF-токен, single-use (FlowStore.Consume = GETDEL);
//   - nonce — anti-replay id_token;
//   - issuer — только HTTPS (валидация config-слоя + New);
//   - любая причина отказа санитизируется в auth.ErrAuthFailed (anti-oracle);
//     детали — только debug-лог без токенов/секретов.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
)

// defaultAIDClaim — claim, из которого выводится AID, если aid_claim не задан.
// `sub` выбран дефолтом: стабильный непрозрачный идентификатор субъекта у IdP,
// присутствует всегда (обязательный OIDC-claim), не меняется при смене email.
const defaultAIDClaim = "sub"

// defaultGroupsClaim — claim с группами по умолчанию (Keycloak/большинство IdP).
const defaultGroupsClaim = "groups"

// defaultScopes — минимальный набор scope, если не задан: openid обязателен
// (без него IdP не вернёт id_token), profile/email/groups — для claims.
var defaultScopes = []string{gooidc.ScopeOpenID, "profile", "email", "groups"}

// Config — резолвнутая конфигурация OIDC-аутентификатора. ClientSecret уже
// резолвнут из Vault на load-time (ADR-058(e)) — сюда приходит plaintext, НЕ
// *_ref. TLSCA — резолвнутый CA-bundle IdP (опц.).
//
// Поля 1:1 с config.KeeperAuthOIDC (shared/config/keeper.go), но без *_ref.
type Config struct {
	Issuer       string   // https://idp/realms/... (discovery base, HTTPS-only)
	ClientID     string   // OAuth2 client_id (== ожидаемый aud id_token)
	ClientSecret string   // резолвнут из client_secret_ref (может быть пуст для public-client)
	RedirectURL  string   // https://keeper/auth/oidc/callback
	Scopes       []string // openid (обяз.), email, profile, groups
	TLSCA        []byte   // резолвнутый CA-bundle IdP (опц.)
	AIDClaim     string   // claim → AID (дефолт sub)
	GroupsClaim  string   // claim → []groups для role-map (дефолт groups)
}

// FlowStore — server-side store секретов code-flow (nonce + PKCE verifier),
// keyed by state. Объявлен интерфейсом, чтобы oidc-пакет не импортировал
// keeper/internal/redis (layering) и тестировался с in-memory fake.
// *redis.OIDCFlowStore удовлетворяет автоматически (Save/Consume).
type FlowStore interface {
	Save(ctx context.Context, state string, fs FlowState) error
	Consume(ctx context.Context, state string) (FlowState, error)
}

// FlowState — server-side секреты одного flow (зеркало redis.OIDCFlowState).
// Дублируется здесь, чтобы oidc-пакет не зависел от redis-пакета по типам;
// daemon-адаптер конвертирует между ними при wiring.
type FlowState struct {
	Nonce        string
	CodeVerifier string
}

// ErrFlowNotFound — Consume не нашёл записи по state (CSRF / replay / истёкший
// TTL). Реализация FlowStore возвращает её; CompleteLogin маппит в ErrAuthFailed.
var ErrFlowNotFound = errors.New("auth/oidc: flow state not found")

// Authorization — выдаётся при старте login-flow: куда редиректить браузер и
// какой opaque state хранить (для логов endpoint-а). CodeVerifier/Nonce уже
// сохранены в FlowStore, в Authorization их НЕТ (server-side only).
type Authorization struct {
	RedirectTo string // authorization_endpoint IdP с query (client_id/redirect/scope/state/code_challenge)
	State      string // CSRF-токен (для лог-трассы)
}

// Authenticator выполняет OIDC code-flow с PKCE (ADR-058(b)).
type Authenticator struct {
	cfg         Config
	verifier    *gooidc.IDTokenVerifier
	oauth2      *oauth2.Config
	store       FlowStore
	httpClient  *http.Client // кастомный CA IdP (если TLSCA задан); иначе nil → дефолт
	aidClaim    string
	groupsClaim string
	logger      *slog.Logger
}

// New конструирует OIDC-аутентификатор: discovery по issuer (сетевой вызов к
// /.well-known/openid-configuration), построение verifier (JWKS, aud=client_id)
// и oauth2.Config (endpoints из discovery). store обязателен (без него flow
// невозможен — nonce/PKCE-секреты негде хранить).
//
// Валидирует инварианты безопасности (issuer HTTPS-only, client_id/redirect_url
// обязательны) — defense-in-depth поверх semantic-валидации config-слоя.
func New(ctx context.Context, cfg Config, store FlowStore, logger *slog.Logger) (*Authenticator, error) {
	if !strings.HasPrefix(cfg.Issuer, "https://") {
		return nil, fmt.Errorf("auth/oidc: issuer %q must be https://", cfg.Issuer)
	}
	if cfg.ClientID == "" {
		return nil, errors.New("auth/oidc: client_id is required")
	}
	if cfg.RedirectURL == "" {
		return nil, errors.New("auth/oidc: redirect_url is required")
	}
	if store == nil {
		return nil, errors.New("auth/oidc: flow store is required (nonce/PKCE persistence)")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Кастомный CA IdP (опц.): и discovery, и JWKS-fetch, и token-exchange должны
	// ходить через него. go-oidc/oauth2 берут *http.Client из context (oidc.
	// ClientContext / oauth2 context-key).
	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	discoveryCtx := ctx
	if httpClient != nil {
		discoveryCtx = gooidc.ClientContext(ctx, httpClient)
	}

	provider, err := gooidc.NewProvider(discoveryCtx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth/oidc: discovery %q: %w", cfg.Issuer, err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultScopes
	}
	oa := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  cfg.RedirectURL,
		Scopes:       scopes,
	}

	a := &Authenticator{
		cfg:         cfg,
		verifier:    provider.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth2:      oa,
		store:       store,
		httpClient:  httpClient,
		aidClaim:    orDefault(cfg.AIDClaim, defaultAIDClaim),
		groupsClaim: orDefault(cfg.GroupsClaim, defaultGroupsClaim),
		logger:      logger,
	}
	return a, nil
}

// buildHTTPClient собирает *http.Client с кастомным CA IdP, если TLSCA задан;
// иначе возвращает nil (используется дефолтный транспорт с системными CA).
func buildHTTPClient(cfg Config) (*http.Client, error) {
	if len(cfg.TLSCA) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.TLSCA) {
		return nil, errors.New("auth/oidc: tls.ca_ref does not contain a valid PEM certificate")
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		},
	}, nil
}

// Method реализует auth.Authenticator: возвращает "oidc" (ADR-058(a)).
func (a *Authenticator) Method() string { return "oidc" }

// BeginLogin стартует authorization-code flow: генерирует state/nonce/PKCE-
// verifier, сохраняет {state→(nonce,verifier)} в FlowStore и возвращает
// Authorization с redirect-URL (S256 code_challenge ушёл в URL, verifier — нет).
func (a *Authenticator) BeginLogin(ctx context.Context) (Authorization, error) {
	state, err := randToken()
	if err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: generate state: %w", err)
	}
	nonce, err := randToken()
	if err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: generate nonce: %w", err)
	}
	verifier := oauth2.GenerateVerifier()

	if err := a.store.Save(ctx, state, FlowState{Nonce: nonce, CodeVerifier: verifier}); err != nil {
		return Authorization{}, fmt.Errorf("auth/oidc: persist flow state: %w", err)
	}

	url := a.oauth2.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier), // PKCE S256 (обязателен, ADR-058)
	)
	return Authorization{RedirectTo: url, State: state}, nil
}

// CompleteLogin завершает flow по (code, state): Consume state (CSRF + single-use),
// code-exchange с PKCE verifier, ВАЛИДАЦИЯ id_token через verifier (JWKS/iss/aud/
// exp), сверка nonce, извлечение claims → auth.ExternalIdentity. Любая ошибка
// наружу — auth.ErrAuthFailed (anti-oracle); детали — debug-лог без токенов.
func (a *Authenticator) CompleteLogin(ctx context.Context, code, state string) (auth.ExternalIdentity, error) {
	if code == "" || state == "" {
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 1. Consume state из store: единственный анти-CSRF + single-use (GETDEL).
	// Неизвестный/потреблённый/истёкший state → отказ.
	fs, err := a.store.Consume(ctx, state)
	if err != nil {
		a.debugFail("consume state", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	exchangeCtx := ctx
	if a.httpClient != nil {
		exchangeCtx = gooidc.ClientContext(ctx, a.httpClient)
	}

	// 2. Code-exchange c PKCE verifier. Без verifier (perfect-CSRF / перехват
	// code) IdP отвергнет обмен — PKCE-enforcement.
	tok, err := a.oauth2.Exchange(exchangeCtx, code, oauth2.VerifierOption(fs.CodeVerifier))
	if err != nil {
		a.debugFail("code exchange", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 3. Извлечь id_token из ответа token-endpoint.
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		a.logger.Debug("auth/oidc: token response has no id_token")
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 4. ВАЛИДАЦИЯ id_token: подпись через JWKS, iss==issuer, aud==client_id,
	// exp/iat — всё делает go-oidc verifier.
	idToken, err := a.verifier.Verify(exchangeCtx, rawID)
	if err != nil {
		a.debugFail("verify id_token", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 5. Anti-replay: nonce из id_token должен совпасть с сохранённым.
	if idToken.Nonce == "" || idToken.Nonce != fs.Nonce {
		a.logger.Debug("auth/oidc: nonce mismatch")
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	// 6. Извлечь claims → ExternalIdentity. Маппинг на AID+роли — auth.Mapper.
	return a.identityFromClaims(idToken)
}

// identityFromClaims разбирает claims id_token в auth.ExternalIdentity: AID из
// aid_claim, группы из groups_claim, опц. email/preferred_username.
func (a *Authenticator) identityFromClaims(idToken *gooidc.IDToken) (auth.ExternalIdentity, error) {
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		a.debugFail("decode claims", err)
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	aid := strings.ToLower(claimString(claims, a.aidClaim))
	if aid == "" {
		a.logger.Debug("auth/oidc: aid claim empty", slog.String("aid_claim", a.aidClaim))
		return auth.ExternalIdentity{}, auth.ErrAuthFailed
	}

	return auth.ExternalIdentity{
		Subject:  idToken.Subject,
		AID:      aid,
		Email:    claimString(claims, "email"),
		Username: claimString(claims, "preferred_username"),
		Groups:   claimStrings(claims, a.groupsClaim),
		Claims:   claims,
	}, nil
}

// debugFail логирует причину отказа на debug-уровне. НИКОГДА не логирует
// токены/code/secret — только тег этапа и текст ошибки.
func (a *Authenticator) debugFail(stage string, err error) {
	a.logger.Debug("auth/oidc: authentication failed",
		slog.String("stage", stage), slog.Any("error", err))
}

// randToken — 256-битный криптослучайный токен (state/nonce), base64url без
// паддинга (URL-safe для query/cookie).
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// claimString достаёт строковый claim; отсутствует/не строка → "".
func claimString(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}

// claimStrings достаёт claim-список строк. Поддерживает []any (JSON-массив,
// типичный для groups) и единичную строку (некоторые IdP отдают одну группу
// скаляром).
func claimStrings(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// compile-time assertion: *Authenticator реализует auth.Authenticator.
var _ auth.Authenticator = (*Authenticator)(nil)
