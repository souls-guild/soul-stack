package api

// Обмен session-cookie на короткий Bearer (NIM-77, ADR-058, Вариант B):
// POST /auth/token — сервер читает HttpOnly-cookie `soul_session` (внутренний
// JWT), верифицирует ТЕМ ЖЕ verifier-ом, что и Bearer на /v1, и выпускает
// короткоживущий Bearer в JSON-теле. SPA кладёт его в localStorage и шлёт
// `Authorization: Bearer` на /v1 (RequireJWT/extractToken НЕ трогаются — cookie
// на /v1 по-прежнему не читается). GET /auth/methods — публичный список
// доступных способов логина для формы входа.
//
// Инвариант безопасности: subject/roles/bootstrap_initial выданного токена
// берутся СТРОГО из проверенных claims cookie — ничего из body/query/headers
// (нет privilege-escalation). Обмен в audit НЕ пишется (high-freq refresh).

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// revocationChecker — узкий контракт revoked-чека по in-memory RBAC-снимку
// (map-lookup, не SQL). *rbac.Holder удовлетворяет автоматически (IsRevoked) —
// импорт rbac в api-слой не нужен (паттерн ldapAuthenticator).
type revocationChecker interface {
	IsRevoked(aid string) bool
}

// AuthTokenDeps — зависимости POST /auth/token. При nil в [Deps.AuthToken]
// registerHumaAuthTokenExchange — no-op (opt-in, паттерн LDAPAuthDeps).
type AuthTokenDeps struct {
	Verifier *jwt.Verifier     // тот же verifier, что RequireJWT на /v1
	Issuer   JWTIssuerLogin    // тот же shared issuer, что LDAP/OIDC-логин
	TTL      time.Duration     // короткий TTL выдаваемого Bearer (exchange_ttl)
	Revoked  revocationChecker // nil → revoked-чек пропускается
	Logger   *slog.Logger
}

// AuthMethodsDeps — булевы флаги доступных способов логина для GET /auth/methods
// (UI рисует форму входа). Значение, не указатель: методы всегда доступны
// (роут монтируется безусловно). Только booleans — без IdP-URL/доменов.
type AuthMethodsDeps struct {
	// Password — ВСЕГДА true (решение пользователя Q4, ADR-058): классическая
	// вставка operator-JWT в localStorage доступна независимо от федеративных
	// методов (FE paste-JWT fallback — токен кладётся прямо как Bearer, cookie и
	// обмен не нужны). НЕ привязан к отдельному endpoint; контракт «локальный вход
	// есть всегда».
	Password bool
	LDAP     bool
	OIDC     bool
}

// authTokenInput — huma-input POST /auth/token. Session — HttpOnly-cookie
// soul_session (внутренний JWT); SecFetchSite — defense-in-depth против
// cross-site-обмена. Оба `required:"false"`: отсутствие cookie обрабатывает
// handler (→401), отсутствие Sec-Fetch-Site — легитимно (не-браузерный клиент,
// пропуск), 422 от huma тут недопустим. Body отсутствует — субъект/роли берутся
// ТОЛЬКО из cookie.
type authTokenInput struct {
	Session      string `cookie:"soul_session" required:"false"`
	SecFetchSite string `header:"Sec-Fetch-Site" required:"false"`
}

// AuthTokenReply — тело ответа POST /auth/token: короткий Bearer + его exp.
// Имя НЕ *Response/*TTL (schema-naming guard TestFullSpec_NoTechnicalSchemaNames).
type AuthTokenReply struct {
	Token     string    `json:"token" doc:"короткий Bearer-JWT для Authorization на /v1"`
	ExpiresAt time.Time `json:"expires_at" doc:"момент истечения выданного Bearer"`
}

// authTokenOutput — huma-output с телом [AuthTokenReply] (статус 200).
type authTokenOutput struct {
	Body AuthTokenReply
}

// authMethodsInput — input без полей (публичный запрос без параметров).
type authMethodsInput struct{}

// AuthMethodsReply — доступные способы логина (только booleans). password всегда
// true (локальный вход есть всегда). Имя НЕ *Response (schema-naming guard).
type AuthMethodsReply struct {
	Password bool `json:"password" doc:"локальный логин (всегда доступен)"`
	LDAP     bool `json:"ldap" doc:"федеративный LDAP-логин сконфигурирован"`
	OIDC     bool `json:"oidc" doc:"федеративный OIDC-логин сконфигурирован"`
}

// authMethodsOutput — huma-output с телом [AuthMethodsReply] (статус 200).
type authMethodsOutput struct {
	Body AuthMethodsReply
}

// authTokenExchangeOperation — метаданные POST /auth/token. Path относителен
// chi-группе /auth (полный URL = /auth/token). 429 — anti-bruteforce throttle.
func authTokenExchangeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "authTokenExchange",
		Method:        http.MethodPost,
		Path:          "/token",
		Summary:       "Обмен session-cookie на короткий Bearer",
		Description:   "NIM-77/ADR-058 (Вариант B): читает HttpOnly-cookie soul_session (внутренний JWT), верифицирует тем же verifier, что и Bearer на /v1, и выпускает короткоживущий Bearer в JSON-теле. Субъект/роли — строго из проверенных claims cookie. В audit не пишется.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// authMethodsOperation — метаданные GET /auth/methods (публичный, без throttle).
func authMethodsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "authMethods",
		Method:        http.MethodGet,
		Path:          "/methods",
		Summary:       "Доступные способы логина",
		Description:   "Публичный список способов входа для формы логина UI (password всегда true; ldap/oidc — по конфигурации keeper). Только booleans, без IdP-URL/доменов.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
	}
}

// registerHumaAuthTokenExchange монтирует POST /auth/token через huma. d nil →
// no-op (opt-in-домен). Handler: Sec-Fetch-guard → verify cookie → revoked-чек →
// TTL-cap на cookie.exp → Issue. Ошибки санитизированы (anti-oracle).
func registerHumaAuthTokenExchange(humaAPI huma.API, d *AuthTokenDeps) {
	if d == nil {
		return
	}
	huma.Register(humaAPI, authTokenExchangeOperation(), func(_ context.Context, in *authTokenInput) (*authTokenOutput, error) {
		// 1. Sec-Fetch defense-in-depth: отклоняем ТОЛЬКО явный cross-site
		// (пусто/same-origin/same-site/none — пропуск; не-браузерный клиент без
		// заголовка легитимен).
		if in.SecFetchSite == "cross-site" {
			return nil, authTokenForbidden()
		}
		// 2. Нет cookie → generic 401 (браузер без сессии).
		if in.Session == "" {
			return nil, authTokenUnauthenticated("")
		}
		// 3. Верификация cookie тем же verifier, что и Bearer. detail — только
		// public-safe [jwt.ClassifyVerifyErr] (raw err не протаскиваем, anti-oracle).
		claims, err := d.Verifier.Verify(in.Session)
		if err != nil {
			return nil, authTokenUnauthenticated(jwt.ClassifyVerifyErr(err))
		}
		// 4. Revoked-чек по in-memory RBAC-снимку (map-lookup): ревокнутый Архонт
		// не обменяет ещё живую cookie на новый Bearer.
		if d.Revoked != nil && d.Revoked.IsRevoked(claims.Subject) {
			return nil, authTokenUnauthenticated("")
		}
		// 5. TTL-cap на cookie.exp: выданный Bearer не переживёт исходную сессию.
		ttl := d.TTL
		if rem := time.Until(claims.ExpiresAt); rem < ttl {
			ttl = rem
		}
		// defensive-guard на будущий leeway: сейчас НЕДОСТИЖИМО — Verify без leeway
		// уже отбил протухшую cookie (ErrExpiredToken) на шаге 3, значит
		// claims.ExpiresAt строго в будущем и rem>0. Ветка оживёт, только если в
		// Verify добавят clock-leeway (тогда cookie с exp в прошлом на ≤leeway
		// прошла бы, и rem мог стать ≤0). Явный 401 вместо Issue(ttl≤0)→error/500.
		if ttl <= 0 {
			return nil, authTokenUnauthenticated("")
		}
		// 6. Выпуск. Subject/roles/bootstrapInitial — СТРОГО из проверенных claims
		// (нет privilege-escalation через body/query/headers).
		token, err := d.Issuer.Issue(claims.Subject, claims.Roles, ttl, claims.BootstrapInitial)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/token exchange: issue JWT failed",
					slog.String("aid", claims.Subject), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}
		// 7. expires_at — из ФАКТИЧЕСКОГО выданного токена (self-verify), НЕ
		// time.Now().Add(ttl): Issue берёт собственный time.Now() и усекает exp до
		// секунды (jwt/v5), поэтому повторный расчёт дал бы exp на ≤~1s ПОЗЖЕ
		// реального — клиент планировал бы re-exchange по неверному дедлайну и в узком
		// окне словил бы 401 на /v1. Verify заодно self-check: никогда не отдаём
		// токен, который сами не верифицируем (HS256, микросекунды).
		verified, err := d.Verifier.Verify(token)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/token exchange: self-verify выданного токена не прошёл",
					slog.String("aid", claims.Subject), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}
		return &authTokenOutput{Body: AuthTokenReply{Token: token, ExpiresAt: verified.ExpiresAt}}, nil
	})
}

// registerHumaAuthMethods монтирует публичный GET /auth/methods. НЕ opt-in
// (методы всегда объявлены): значение-deps, no-op-ветки нет.
func registerHumaAuthMethods(humaAPI huma.API, d AuthMethodsDeps) {
	huma.Register(humaAPI, authMethodsOperation(), func(_ context.Context, _ *authMethodsInput) (*authMethodsOutput, error) {
		return &authMethodsOutput{Body: AuthMethodsReply{Password: d.Password, LDAP: d.LDAP, OIDC: d.OIDC}}, nil
	})
}

// authTokenUnauthenticated — generic 401 problem+json. detail пуст → нейтральное
// сообщение (anti-oracle: не различаем «нет cookie»/«revoked»/«ttl истёк»);
// непустой detail приходит только из [jwt.ClassifyVerifyErr] (public-safe, тот
// же набор, что RequireJWT уже отдаёт для Bearer — новой oracle-поверхности нет).
func authTokenUnauthenticated(detail string) huma.StatusError {
	if detail == "" {
		detail = "authentication required"
	}
	return humaProblemError{Details: problemWithStatus(problem.TypeUnauthenticated, http.StatusUnauthorized, detail)}
}

// authTokenForbidden — 403 на явный cross-site обмен (санитизировано).
func authTokenForbidden() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "cross-site token exchange rejected")}
}

// authTokenSpecStub — non-nil заглушка зависимостей для dump-спеки (handler при
// dump не вызывается, нужен лишь non-nil для регистрации операции).
func authTokenSpecStub() *AuthTokenDeps { return &AuthTokenDeps{} }

// authMethodsSpecStub — value-заглушка для dump-спеки.
func authMethodsSpecStub() AuthMethodsDeps { return AuthMethodsDeps{} }
