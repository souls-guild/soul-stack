package api

// Федеративная OIDC-аутентификация операторов: GET /auth/oidc/login + GET
// /auth/oidc/callback (ADR-058, стадия 2). ROOT-mount ВНЕ /v1 (parity
// /auth/ldap/login): публичные браузерные redirect-эндпоинты (JWT ещё нет —
// RequireJWT неприменим).
//
// /auth/oidc/login   → 302 на authorization_endpoint IdP (state+nonce+PKCE);
// /auth/oidc/callback → валидация id_token → внутренний JWT в HttpOnly-cookie →
//                       302 на UI (Set-Cookie + Location).
//
// Audit пишет САМ handler (operator.login после выпуска JWT, как у LDAP).
// FULL-TYPED huma-операции (паттерн ADR-054): query-input → header-output
// (Location + Set-Cookie). Ошибки санитизированы (anti-oracle).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	oidcauth "github.com/souls-guild/soul-stack/keeper/internal/auth/oidc"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// oidcCallbackSuccessRedirect — куда редиректить браузер после успешного логина.
// `/ui` — встроенный UI (ADR-055); cookie уже выставлен, UI подхватит сессию.
const oidcCallbackSuccessRedirect = "/ui"

// OIDCAuthDeps — зависимости OIDC-эндпоинтов. nil → не монтируются (opt-in,
// как LDAPAuth): keeper.yml::auth.oidc не задан → способ логина недоступен.
type OIDCAuthDeps struct {
	Authenticator oidcAuthenticator
	Mapper        auth.Mapper
	Issuer        JWTIssuerLogin
	TTL           time.Duration
	Audit         audit.Writer
	Logger        *slog.Logger
}

// oidcAuthenticator — узкий контракт OIDC-аутентификатора (избегаем импорта
// тяжёлого oidc-пакета в api-слой по реализации; *oidc.Authenticator
// удовлетворяет автоматически).
type oidcAuthenticator interface {
	BeginLogin(ctx context.Context) (oidcauth.Authorization, error)
	CompleteLogin(ctx context.Context, code, state string) (auth.ExternalIdentity, error)
}

// --- /auth/oidc/login ---

// oidcLoginInput — input без полей (login-старт не требует параметров).
type oidcLoginInput struct{}

// oidcLoginOutput — 302 на IdP. Тело пустое; Location — authorization-URL.
type oidcLoginOutput struct {
	Status   int    `json:"-"`
	Location string `header:"Location" json:"-"`
}

func oidcLoginOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "oidcLogin",
		Method:        http.MethodGet,
		Path:          "/oidc/login",
		Summary:       "Старт OIDC-логина оператора",
		Description:   "Федеративная аутентификация (ADR-058): генерирует state+nonce+PKCE и редиректит (302) на authorization_endpoint внешнего IdP.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		// 429 — anti-bruteforce throttle (AuthLoginLimit middleware, HIGH-3:
		// гасит flow-state-flood на старте login-flow).
		Errors: []int{http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// --- /auth/oidc/callback ---

// oidcCallbackInput — query от IdP-редиректа: code + state (+ опц. error).
type oidcCallbackInput struct {
	Code  string `query:"code" doc:"authorization code от IdP"`
	State string `query:"state" doc:"opaque CSRF-state, выданный на /auth/oidc/login"`
	Error string `query:"error" doc:"код ошибки от IdP (если аутентификация отклонена)"`
}

// oidcCallbackOutput — 302 на UI + Set-Cookie с внутренним JWT.
type oidcCallbackOutput struct {
	Status    int    `json:"-"`
	Location  string `header:"Location" json:"-"`
	SetCookie string `header:"Set-Cookie" json:"-"`
}

func oidcCallbackOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "oidcCallback",
		Method:        http.MethodGet,
		Path:          "/oidc/callback",
		Summary:       "OIDC-callback оператора",
		Description:   "Валидирует id_token (JWKS-подпись/iss/aud/exp/nonce), маппит на operators(aid)+роли, выпускает внутренний JWT в HttpOnly+Secure cookie и редиректит (302) в UI. Ошибка валидации/маппинга → 401/403.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		// 429 — anti-bruteforce throttle/lockout (AuthLoginLimit middleware, HIGH-3).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// registerHumaOIDCLogin монтирует GET /auth/oidc/login + /callback через huma.
// d nil → no-op (opt-in-домен).
func registerHumaOIDCLogin(humaAPI huma.API, d *OIDCAuthDeps) {
	if d == nil {
		return
	}

	huma.Register(humaAPI, oidcLoginOperation(), func(ctx context.Context, _ *oidcLoginInput) (*oidcLoginOutput, error) {
		authz, err := d.Authenticator.BeginLogin(ctx)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/oidc login: begin failed", slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}
		return &oidcLoginOutput{Status: http.StatusFound, Location: authz.RedirectTo}, nil
	})

	huma.Register(humaAPI, oidcCallbackOperation(), func(ctx context.Context, in *oidcCallbackInput) (*oidcCallbackOutput, error) {
		// IdP вернул error (user denied / IdP-ошибка) → отказ без утечки причины.
		if in.Error != "" {
			if d.Logger != nil {
				d.Logger.Debug("auth/oidc callback: idp returned error", slog.String("error", in.Error))
			}
			return nil, oidcLoginProblem(auth.ErrAuthFailed)
		}

		ext, err := d.Authenticator.CompleteLogin(ctx, in.Code, in.State)
		if err != nil {
			return nil, oidcLoginProblem(err)
		}
		mapped, err := d.Mapper.Map(ctx, ext)
		if err != nil {
			return nil, oidcLoginProblem(err)
		}
		token, err := d.Issuer.Issue(mapped.AID, mapped.Roles, d.TTL, false)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/oidc callback: issue JWT failed", slog.String("aid", mapped.AID), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}

		// ЕДИНАЯ cookie-фабрика (newSessionCookie): SameSite=Strict симметрично LDAP
		// (MED-фикс рассинхрона, ADR-058(g)). Strict безопасен для callback-а:
		// cookie СТАВИТСЯ на ответе callback-а, а ОТПРАВЛЯЕТСЯ на последующей
		// same-site навигации /ui (302). См. doc newSessionCookie.
		cookie := newSessionCookie(token, d.TTL)

		if d.Audit != nil {
			ev := &audit.Event{
				AuditID:   audit.NewULID(),
				EventType: audit.EventOperatorLogin,
				Source:    audit.SourceAPI,
				ArchonAID: mapped.AID,
				Payload: map[string]any{
					"method":      "oidc",
					"aid":         mapped.AID,
					"provisioned": mapped.Provisioned,
				},
			}
			if err := d.Audit.Write(ctx, ev); err != nil && d.Logger != nil {
				d.Logger.Error("auth/oidc callback: audit write failed (login succeeded)",
					slog.String("aid", mapped.AID), slog.Any("error", err))
			}
		}

		return &oidcCallbackOutput{
			Status:    http.StatusFound,
			Location:  oidcCallbackSuccessRedirect,
			SetCookie: cookie.String(),
		}, nil
	})
}

// oidcLoginProblem маппит sentinel-ошибки auth в problem+json (parity
// ldapLoginProblem). Anti-oracle: причина отказа наружу не утекает.
func oidcLoginProblem(err error) huma.StatusError {
	switch {
	case errors.Is(err, auth.ErrAuthFailed):
		return humaProblemError{Details: problemWithStatus(problem.TypeUnauthenticated, http.StatusUnauthorized, "authentication failed")}
	case errors.Is(err, auth.ErrNoRoleMapping):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "no mapped group")}
	case errors.Is(err, auth.ErrOperatorRevoked):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "operator revoked")}
	case errors.Is(err, auth.ErrProvisioningDisabled):
		return humaProblemError{Details: problemWithStatus(problem.TypeProvisioningMethodDisabled, http.StatusForbidden, "operator provisioning is disabled for this method by policy")}
	default:
		return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
	}
}

// HumaOIDCSpecYAML — OpenAPI-фрагмент OIDC-роутов (parity HumaAuthSpecYAML).
func HumaOIDCSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaOIDCLogin(api, oidcAuthSpecStub())
		return nil
	})
}

// oidcAuthSpecStub — non-nil заглушка зависимостей для dump-спеки.
func oidcAuthSpecStub() *OIDCAuthDeps {
	return &OIDCAuthDeps{}
}
