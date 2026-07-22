package api

// Federated OIDC authentication for operators: GET /auth/oidc/login + GET
// /auth/oidc/callback (ADR-058, stage 2). ROOT-mount OUTSIDE /v1 (parity with
// /auth/ldap/login): public browser redirect endpoints (no JWT yet —
// RequireJWT does not apply).
//
// /auth/oidc/login    → 302 to the IdP authorization_endpoint (state+nonce+PKCE);
// /auth/oidc/callback → id_token validation → internal JWT in an HttpOnly cookie →
//                        302 to the UI (Set-Cookie + Location).
//
// Audit is written by the handler ITSELF (operator.login after issuing the JWT,
// same as LDAP). FULL-TYPED huma operations (ADR-054 pattern): query-input →
// header-output (Location + Set-Cookie). Errors are sanitized (anti-oracle).

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

// oidcCallbackSuccessRedirect — where to redirect the browser after a successful login.
// `/ui` — the built-in UI (ADR-055); the cookie is already set, the UI will pick up the session.
const oidcCallbackSuccessRedirect = "/ui"

// OIDCAuthDeps — dependencies of the OIDC endpoints. nil → not mounted (opt-in,
// same as LDAPAuth): keeper.yml::auth.oidc not set → this login method is unavailable.
type OIDCAuthDeps struct {
	Authenticator oidcAuthenticator
	Mapper        auth.Mapper
	Issuer        JWTIssuerLogin
	TTL           time.Duration
	Audit         audit.Writer
	Logger        *slog.Logger
}

// oidcAuthenticator — a narrow OIDC authenticator contract (avoids importing the
// heavy oidc package into the api layer for the implementation; *oidc.Authenticator
// satisfies it automatically).
type oidcAuthenticator interface {
	BeginLogin(ctx context.Context) (oidcauth.Authorization, error)
	CompleteLogin(ctx context.Context, code, state string) (auth.ExternalIdentity, error)
}

// --- /auth/oidc/login ---

// oidcLoginInput — input with no fields (starting login requires no parameters).
type oidcLoginInput struct{}

// oidcLoginOutput — 302 to the IdP. Body is empty; Location is the authorization URL.
type oidcLoginOutput struct {
	Status   int    `json:"-"`
	Location string `header:"Location" json:"-"`
}

func oidcLoginOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "oidcLogin",
		Method:        http.MethodGet,
		Path:          "/oidc/login",
		Summary:       "Start OIDC login for the operator",
		Description:   "Federated authentication (ADR-058): generates state+nonce+PKCE and redirects (302) to the external IdP's authorization_endpoint.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		// 429 — anti-bruteforce throttle (AuthLoginLimit middleware, HIGH-3:
		// dampens flow-state flood at the start of the login flow).
		Errors: []int{http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// --- /auth/oidc/callback ---

// oidcCallbackInput — query from the IdP redirect: code + state (+ optional error).
type oidcCallbackInput struct {
	Code  string `query:"code" doc:"authorization code from the IdP"`
	State string `query:"state" doc:"opaque CSRF-state, issued at /auth/oidc/login"`
	Error string `query:"error" doc:"error code from the IdP (if authentication was rejected)"`
}

// oidcCallbackOutput — 302 to the UI + Set-Cookie with the internal JWT.
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
		Summary:       "OIDC callback for the operator",
		Description:   "Validates id_token (JWKS signature/iss/aud/exp/nonce), maps to operators(aid)+roles, issues the internal JWT in an HttpOnly+Secure cookie and redirects (302) to the UI. Validation/mapping error -> 401/403.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusFound,
		// 429 — anti-bruteforce throttle/lockout (AuthLoginLimit middleware, HIGH-3).
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// registerHumaOIDCLogin mounts GET /auth/oidc/login + /callback via huma.
// d nil → no-op (opt-in domain).
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
		// IdP returned an error (user denied / IdP error) → reject without leaking the reason.
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

		// A SINGLE cookie factory (newSessionCookie): SameSite=Strict symmetric with LDAP
		// (MED fix for the mismatch, ADR-058(g)). Strict is safe for the callback:
		// the cookie is SET on the callback response and SENT on the subsequent
		// same-site navigation to /ui (302). See the newSessionCookie doc.
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

// oidcLoginProblem maps auth sentinel errors to problem+json (parity with
// ldapLoginProblem). Anti-oracle: the failure reason does not leak externally.
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

// HumaOIDCSpecYAML — the OpenAPI fragment of the OIDC routes (parity with HumaAuthSpecYAML).
func HumaOIDCSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		registerHumaOIDCLogin(api, oidcAuthSpecStub())
		return nil
	})
}

// oidcAuthSpecStub — a non-nil dependency stub for the spec dump.
func oidcAuthSpecStub() *OIDCAuthDeps {
	return &OIDCAuthDeps{}
}
