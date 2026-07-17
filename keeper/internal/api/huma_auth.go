package api

// Federated operator authentication: POST /auth/ldap/login (ADR-058,
// stage 1 LDAP). ROOT-mount OUTSIDE /v1 (parity /healthz): this is a public entry
// (login itself, no JWT yet — RequireJWT is inapplicable). A FULL-TYPED huma operation
// (ADR-054 pattern): typed body input → Authenticator → Mapper → issuer.Issue →
// Set-Cookie (HttpOnly+Secure+SameSite=Strict, no JSON token in the body).
//
// Audit is written by the handler ITSELF (operator.login after issuing the JWT), the huma-audit
// middleware is NOT attached — the login event is single, payload without secrets.

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

// sessionCookieName — the name of the HttpOnly cookie with the internal JWT (ADR-058: cookie-only
// delivery, no JSON token in the body). `soul_session` — our own session name.
const sessionCookieName = "soul_session"

// newSessionCookie builds the Set-Cookie with the internal JWT — the SINGLE
// point shared by LDAP and OIDC (ADR-058(g)/(#4): symmetry of login methods is
// mandatory). HttpOnly+Secure+`SameSite=Strict`+`Path=/auth`.
//
// Path=/auth (narrowed from `/`, NIM-77): the only server-side reader of the
// cookie is POST /auth/token (exchange for a short-lived Bearer, Option B). The
// browser sends the cookie ONLY to `/auth/*`, not to /v1//mcp//docs — narrowing
// the leak surface of the 24h credential (the cookie doesn't get attached to
// every API request).
//
// SameSite=Strict is safe for the OIDC callback too (MED fix for the Lax↔Strict
// desync, 2026-06-24): SameSite restricts SENDING the cookie on a cross-site
// request, not SETTING it. On a cross-site top-level redirect from the IdP we
// SET the cookie (Set-Cookie on the callback response), we don't read it; the
// next step is a same-site top-level navigation to `/ui` (302 Location). That
// navigation does NOT send the /auth-cookie (Path doesn't match), but it doesn't
// need to: the UI session is bootstrapped via POST /auth/token (exchange for a
// Bearer).
func newSessionCookie(token string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/auth",
		HttpOnly: true,
		Secure:   true, // TLS-required perimeter (ADR-002 mTLS/HTTPS)
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(ttl),
	}
}

// LDAPAuthDeps — dependencies of the LDAP-login endpoint. When [Deps.LDAPAuth] is
// nil the endpoint is not mounted (opt-in pattern, like pushH/errandH).
type LDAPAuthDeps struct {
	Authenticator ldapAuthenticator
	Mapper        auth.Mapper
	Issuer        JWTIssuerLogin
	TTL           time.Duration
	Audit         audit.Writer
	Logger        *slog.Logger
}

// ldapAuthenticator — a narrow contract for the LDAP authenticator (avoids importing
// keeper/internal/auth/ldap into the api layer; the real *ldap.Authenticator
// satisfies it automatically).
type ldapAuthenticator interface {
	Authenticate(ctx context.Context, username, password string) (auth.ExternalIdentity, error)
}

// JWTIssuerLogin — a narrow issuer contract (parity bootstrap.JWTIssuer): issuing
// the internal JWT after federated authentication.
type JWTIssuerLogin interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// ldapLoginInput — huma input for POST /auth/ldap/login. Body — credentials.
type ldapLoginInput struct {
	Body LDAPLoginRequest
}

// LDAPLoginRequest — the Go shape of the login body (source of the schema AND validation).
// Password carries format:"password" (UI masking); it is NEVER logged and never
// put into audit.
type LDAPLoginRequest struct {
	Username string `json:"username" minLength:"1" doc:"username for LDAP search-bind"`
	Password string `json:"password" format:"password" minLength:"1" doc:"password (never logged, never returned)"`
}

// ldapLoginOutput — huma output. The body is empty (ADR-058: no JSON token);
// SetCookie — the Set-Cookie header with the internal JWT (huma emits the header from
// a field tagged `header:"Set-Cookie"`, the same mechanism as Location in cadence).
type ldapLoginOutput struct {
	Status    int    `json:"-"`
	SetCookie string `header:"Set-Cookie" json:"-"`
}

// ldapLoginOperation — huma.Operation metadata. Path = "/ldap/login" —
// RELATIVE to the /auth chi group on which the huma.API is mounted (full URL
// = /auth/ldap/login). DefaultStatus 204 (no body — the token is in the cookie).
func ldapLoginOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "ldapLogin",
		Method:        http.MethodPost,
		Path:          "/ldap/login",
		Summary:       "Operator login via LDAP",
		Description:   "Federated authentication (ADR-058): LDAP search-bind -> mapping onto operators(aid)+roles -> internal JWT in HttpOnly+Secure cookie. Response body is empty.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusNoContent,
		// 429 — anti-bruteforce throttle/lockout (AuthLoginLimit middleware, HIGH-3).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// registerHumaLDAPLogin mounts POST /auth/ldap/login via huma. d nil →
// no-op (opt-in domain). Handler: Authenticate → Map → Issue → Set-Cookie →
// audit operator.login. Errors are sanitized (anti-oracle): ErrAuthFailed→401
// without a reason, ErrNoRoleMapping→403, ErrOperatorRevoked→403.
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

		cookie := newSessionCookie(token, d.TTL)

		// audit operator.login (after issuing the JWT). WITHOUT password/bind-creds;
		// groups are not secret, but for hygiene we put only method/aid/provisioned.
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

// ldapLoginProblem maps auth sentinel errors into problem+json. Anti-oracle:
// the bad-credentials reason does not leak outward (401 without a detail reason).
func ldapLoginProblem(err error) huma.StatusError {
	switch {
	case errors.Is(err, auth.ErrAuthFailed):
		return humaProblemError{Details: problemWithStatus(problem.TypeUnauthenticated, http.StatusUnauthorized, "authentication failed")}
	case errors.Is(err, auth.ErrNoRoleMapping):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "no mapped group")}
	case errors.Is(err, auth.ErrOperatorRevoked):
		return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "operator revoked")}
	// ErrProvisioningDisabled — the provisioning_allowed_methods policy forbade
	// auto-provision via this method (ADR-058 Part B). 403 with a meaningful detail
	// (NOT a sanitized 401): this is a policy denial, not bad-credentials — anti-oracle
	// is inapplicable (the fact "the method is off" reveals no one's secrets).
	case errors.Is(err, auth.ErrProvisioningDisabled):
		return humaProblemError{Details: problemWithStatus(problem.TypeProvisioningMethodDisabled, http.StatusForbidden, "operator provisioning is disabled for this method by policy")}
	default:
		return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
	}
}

// problemWithStatus — problem.Details with an explicit HTTP status (problem.New takes
// the default from the type table; auth errors need an exact 401/403).
func problemWithStatus(typ string, status int, detail string) problem.Details {
	d := problem.New(typ, "", detail)
	d.Status = status
	return d
}

// newHumaAuthAPI builds a huma.API over the /auth chi group (parity
// newHumaCadenceAPI, WITHOUT audit wiring — login writes audit itself).
func newHumaAuthAPI(r chi.Router) huma.API {
	return newHumaCadenceAPI(r)
}

// HumaAuthSpecYAML — the OpenAPI fragment of the auth routes as YAML (a hook for the spec-merge
// target and a guard test; parity HumaCadenceSpecYAML). The register closure —
// a single dump-vs-mount path.
func HumaAuthSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaLDAPLogin(api, ldapAuthSpecStub())
		return nil
	})
}

// ldapAuthSpecStub — a non-nil dependency stub for the dump spec (the handler is not
// called during dump, only a non-nil is needed to register the operation).
func ldapAuthSpecStub() *LDAPAuthDeps {
	return &LDAPAuthDeps{}
}
