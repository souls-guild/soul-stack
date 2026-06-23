package api

// Федеративная аутентификация операторов: POST /auth/ldap/login (ADR-058,
// стадия 1 LDAP). ROOT-mount ВНЕ /v1 (parity /healthz): это публичный вход
// (сам логин, JWT ещё нет — RequireJWT неприменим). FULL-TYPED huma-операция
// (паттерн ADR-054): typed body input → Authenticator → Mapper → issuer.Issue →
// Set-Cookie (HttpOnly+Secure+SameSite=Strict, JSON-токена в теле НЕТ).
//
// Audit пишет САМ handler (operator.login после выпуска JWT), huma-audit-
// middleware НЕ навешан — login-событие одно, payload без секретов.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sessionCookieName — имя HttpOnly-cookie с внутренним JWT (ADR-058: cookie-only
// доставка, JSON-токена в теле нет). `soul_session` — собственное имя сессии.
const sessionCookieName = "soul_session"

// LDAPAuthDeps — зависимости endpoint-а LDAP-логина. При nil-значении в
// [Deps.LDAPAuth] endpoint не монтируется (opt-in-паттерн, как pushH/errandH).
type LDAPAuthDeps struct {
	Authenticator ldapAuthenticator
	Mapper        auth.Mapper
	Issuer        JWTIssuerLogin
	TTL           time.Duration
	Audit         audit.Writer
	Logger        *slog.Logger
}

// ldapAuthenticator — узкий контракт LDAP-аутентификатора (избегаем импорта
// keeper/internal/auth/ldap в api-слой; реальный *ldap.Authenticator
// удовлетворяет автоматически).
type ldapAuthenticator interface {
	Authenticate(ctx context.Context, username, password string) (auth.ExternalIdentity, error)
}

// JWTIssuerLogin — узкий контракт issuer-а (parity bootstrap.JWTIssuer): выпуск
// внутреннего JWT после федеративной аутентификации.
type JWTIssuerLogin interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// ldapLoginInput — huma-input POST /auth/ldap/login. Body — credentials.
type ldapLoginInput struct {
	Body LDAPLoginRequest
}

// LDAPLoginRequest — Go-форма тела логина (источник схемы И валидации).
// Password несёт format:"password" (UI-маскинг); НИКОГДА не логируется и не
// кладётся в audit.
type LDAPLoginRequest struct {
	Username string `json:"username" minLength:"1" doc:"имя пользователя для LDAP search-bind"`
	Password string `json:"password" format:"password" minLength:"1" doc:"пароль (не логируется, не возвращается)"`
}

// ldapLoginOutput — huma-output. Тело пустое (ADR-058: JSON-токена нет);
// SetCookie — Set-Cookie заголовок с внутренним JWT (huma эмитит header из
// поля с тегом `header:"Set-Cookie"`, тот же механизм, что Location у cadence).
type ldapLoginOutput struct {
	Status    int    `json:"-"`
	SetCookie string `header:"Set-Cookie" json:"-"`
}

// ldapLoginOperation — метаданные huma.Operation. Path = "/ldap/login" —
// ОТНОСИТЕЛЬНЫЙ к chi-группе /auth, на которой смонтирован huma.API (полный URL
// = /auth/ldap/login). DefaultStatus 204 (нет тела — токен в cookie).
func ldapLoginOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "ldapLogin",
		Method:        http.MethodPost,
		Path:          "/ldap/login",
		Summary:       "Логин оператора через LDAP",
		Description:   "Федеративная аутентификация (ADR-058): LDAP search-bind → маппинг на operators(aid)+роли → внутренний JWT в HttpOnly+Secure cookie. Тело ответа пустое.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError},
	}
}

// registerHumaLDAPLogin монтирует POST /auth/ldap/login через huma. d nil →
// no-op (opt-in-домен). Handler: Authenticate → Map → Issue → Set-Cookie →
// audit operator.login. Ошибки санитизированы (anti-oracle): ErrAuthFailed→401
// без причины, ErrNoRoleMapping→403, ErrOperatorRevoked→403.
func registerHumaLDAPLogin(humaAPI huma.API, d *LDAPAuthDeps) {
	if d == nil {
		return
	}
	huma.Register(humaAPI, ldapLoginOperation(), func(ctx context.Context, in *ldapLoginInput) (*ldapLoginOutput, error) {
		ext, err := d.Authenticator.Authenticate(ctx, in.Body.Username, in.Body.Password)
		if err != nil {
			return nil, ldapLoginProblem(err)
		}
		mapped, err := d.Mapper.Map(ctx, ext)
		if err != nil {
			return nil, ldapLoginProblem(err)
		}
		token, err := d.Issuer.Issue(mapped.AID, mapped.Roles, d.TTL, false)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/ldap login: issue JWT failed", slog.String("aid", mapped.AID), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}

		cookie := &http.Cookie{
			Name:     sessionCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   true, // TLS-required периметр (ADR-002 mTLS/HTTPS)
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Now().Add(d.TTL),
		}

		// audit operator.login (после выпуска JWT). БЕЗ пароля/bind-creds;
		// группы — не секрет, но для гигиены кладём только method/aid/provisioned.
		if d.Audit != nil {
			ev := &audit.Event{
				AuditID:   audit.NewULID(),
				EventType: audit.EventOperatorLogin,
				Source:    audit.SourceAPI,
				ArchonAID: mapped.AID,
				Payload: map[string]any{
					"method":      "ldap",
					"aid":         mapped.AID,
					"provisioned": mapped.Provisioned,
				},
			}
			if err := d.Audit.Write(ctx, ev); err != nil && d.Logger != nil {
				d.Logger.Error("auth/ldap login: audit write failed (login succeeded)",
					slog.String("aid", mapped.AID), slog.Any("error", err))
			}
		}

		return &ldapLoginOutput{Status: http.StatusNoContent, SetCookie: cookie.String()}, nil
	})
}

// ldapLoginProblem маппит sentinel-ошибки auth в problem+json. Anti-oracle:
// причина bad-credentials наружу не утекает (401 без detail-причины).
func ldapLoginProblem(err error) huma.StatusError {
	switch {
	case errors.Is(err, auth.ErrAuthFailed):
		return humaProblemError{Details: problemWithStatus(problem.TypeUnauthenticated, http.StatusUnauthorized, "authentication failed")}
	case errors.Is(err, auth.ErrNoRoleMapping):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "no mapped group")}
	case errors.Is(err, auth.ErrOperatorRevoked):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "operator revoked")}
	// ErrProvisioningDisabled — политика provisioning_allowed_methods запретила
	// auto-provision этим методом (ADR-058 Часть B). 403 с осмысленным detail
	// (НЕ санитизированный 401): это policy-отказ, не bad-credentials — anti-oracle
	// неприменим (факт «метод выключен» не раскрывает чужих секретов).
	case errors.Is(err, auth.ErrProvisioningDisabled):
		return humaProblemError{Details: problemWithStatus(problem.TypeProvisioningMethodDisabled, http.StatusForbidden, "operator provisioning is disabled for this method by policy")}
	default:
		return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
	}
}

// problemWithStatus — problem.Details с явным HTTP-статусом (problem.New берёт
// дефолт из таблицы типа; auth-ошибкам нужен точный 401/403).
func problemWithStatus(typ string, status int, detail string) problem.Details {
	d := problem.New(typ, "", detail)
	d.Status = status
	return d
}

// newHumaAuthAPI собирает huma.API поверх chi-группы /auth (parity
// newHumaCadenceAPI, БЕЗ audit-навески — login пишет audit сам).
func newHumaAuthAPI(r chi.Router) huma.API {
	return newHumaCadenceAPI(r)
}

// HumaAuthSpecYAML — OpenAPI-фрагмент auth-роутов как YAML (хук спека-мерж-
// таргета и guard-теста; parity HumaCadenceSpecYAML). register-замыкатель —
// единый путь dump-vs-mount.
func HumaAuthSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaLDAPLogin(api, ldapAuthSpecStub())
		return nil
	})
}

// ldapAuthSpecStub — non-nil заглушка зависимостей для dump-спеки (handler при
// dump не вызывается, нужен лишь non-nil для регистрации операции).
func ldapAuthSpecStub() *LDAPAuthDeps {
	return &LDAPAuthDeps{}
}
