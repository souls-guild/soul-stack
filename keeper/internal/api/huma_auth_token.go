package api

// Exchange of session-cookie for a short Bearer (NIM-77, ADR-058, Variant B):
// POST /auth/token — the server reads the HttpOnly cookie `soul_session` (an
// internal JWT), verifies it with the SAME verifier as the Bearer on /v1, and
// issues a short-lived Bearer in the JSON body. The SPA puts it into
// localStorage and sends `Authorization: Bearer` on /v1 (RequireJWT/extractToken
// are NOT touched — the cookie on /v1 is still not read). GET /auth/methods —
// a public list of available login methods for the login form.
//
// Security invariant: subject/roles/bootstrap_initial of the issued token are
// taken STRICTLY from verified cookie claims — nothing from body/query/headers
// (no privilege-escalation). The exchange is NOT written to audit (high-freq refresh).

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// revocationChecker — a narrow contract for the revoked check against the
// in-memory RBAC snapshot (map-lookup, not SQL). *rbac.Holder satisfies it
// automatically (IsRevoked) — no rbac import needed in the api layer (the
// ldapAuthenticator pattern).
type revocationChecker interface {
	IsRevoked(aid string) bool
}

// AuthTokenDeps — dependencies of POST /auth/token. When nil in [Deps.AuthToken],
// registerHumaAuthTokenExchange is a no-op (opt-in, the LDAPAuthDeps pattern).
type AuthTokenDeps struct {
	Verifier *jwt.Verifier     // the same verifier as RequireJWT on /v1
	Issuer   JWTIssuerLogin    // the same shared issuer as the LDAP/OIDC login
	TTL      time.Duration     // short TTL of the issued Bearer (exchange_ttl)
	Revoked  revocationChecker // nil → the revoked check is skipped
	Logger   *slog.Logger
}

// AuthMethodsDeps — boolean flags of available login methods for GET /auth/methods
// (the UI draws the login form). A value, not a pointer: methods are always
// available (the route is mounted unconditionally). Only booleans — no IdP URLs/domains.
type AuthMethodsDeps struct {
	// Password — ALWAYS true (user decision Q4, ADR-058): the classic
	// operator-JWT paste into localStorage is available regardless of federated
	// methods (FE paste-JWT fallback — the token goes in directly as a Bearer, no
	// cookie/exchange needed). NOT tied to a separate endpoint; the contract is
	// "local login always exists".
	Password bool
	LDAP     bool
	OIDC     bool
}

// authTokenInput — huma input for POST /auth/token. Session — the HttpOnly
// cookie soul_session (internal JWT); SecFetchSite — defense-in-depth against
// cross-site exchange. Both `required:"false"`: a missing cookie is handled by
// the handler (→401), a missing Sec-Fetch-Site is legitimate (non-browser
// client, pass), a 422 from huma is unacceptable here. No body — the
// subject/roles are taken ONLY from the cookie.
type authTokenInput struct {
	Session      string `cookie:"soul_session" required:"false"`
	SecFetchSite string `header:"Sec-Fetch-Site" required:"false"`
}

// AuthTokenReply — POST /auth/token response body: a short Bearer + its exp.
// Name is NOT *Response/*TTL (schema-naming guard TestFullSpec_NoTechnicalSchemaNames).
type AuthTokenReply struct {
	Token     string    `json:"token" doc:"short Bearer JWT for Authorization on /v1"`
	ExpiresAt time.Time `json:"expires_at" doc:"expiration moment of the issued Bearer"`
}

// authTokenOutput — huma output with body [AuthTokenReply] (status 200).
type authTokenOutput struct {
	Body AuthTokenReply
}

// authMethodsInput — no fields (a public request with no parameters).
type authMethodsInput struct{}

// AuthMethodsReply — available login methods (booleans only). password is
// always true (local login always exists). Name is NOT *Response (schema-naming guard).
type AuthMethodsReply struct {
	Password bool `json:"password" doc:"local login (always available)"`
	LDAP     bool `json:"ldap" doc:"federated LDAP login is configured"`
	OIDC     bool `json:"oidc" doc:"federated OIDC login is configured"`
}

// authMethodsOutput — huma output with body [AuthMethodsReply] (status 200).
type authMethodsOutput struct {
	Body AuthMethodsReply
}

// authTokenExchangeOperation — metadata for POST /auth/token. Path is relative
// to the /auth chi group (full URL = /auth/token). 429 — anti-bruteforce throttle.
func authTokenExchangeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "authTokenExchange",
		Method:        http.MethodPost,
		Path:          "/token",
		Summary:       "Exchange session-cookie for a short Bearer",
		Description:   "NIM-77/ADR-058 (Variant B): reads the HttpOnly cookie soul_session (internal JWT), verifies it with the same verifier as the Bearer on /v1, and issues a short-lived Bearer in the JSON body. Subject/roles are strictly from verified cookie claims. Not written to audit.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// authMethodsOperation — metadata for GET /auth/methods (public, no throttle).
func authMethodsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "authMethods",
		Method:        http.MethodGet,
		Path:          "/methods",
		Summary:       "Available login methods",
		Description:   "Public list of login methods for the UI login form (password always true; ldap/oidc — depending on keeper config). Booleans only, no IdP URLs/domains.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusOK,
	}
}

// registerHumaAuthTokenExchange mounts POST /auth/token via huma. d nil →
// no-op (opt-in domain). Handler: Sec-Fetch guard → verify cookie → revoked
// check → TTL cap on cookie.exp → Issue. Errors are sanitized (anti-oracle).
func registerHumaAuthTokenExchange(humaAPI huma.API, d *AuthTokenDeps) {
	if d == nil {
		return
	}
	huma.Register(humaAPI, authTokenExchangeOperation(), func(_ context.Context, in *authTokenInput) (*authTokenOutput, error) {
		// 1. Sec-Fetch defense-in-depth: reject ONLY an explicit cross-site
		// (empty/same-origin/same-site/none — pass; a non-browser client without
		// the header is legitimate).
		if in.SecFetchSite == "cross-site" {
			return nil, authTokenForbidden()
		}
		// 2. No cookie → generic 401 (browser without a session).
		if in.Session == "" {
			return nil, authTokenUnauthenticated("")
		}
		// 3. Verify the cookie with the same verifier as the Bearer. detail is
		// only public-safe [jwt.ClassifyVerifyErr] (the raw err is not leaked, anti-oracle).
		claims, err := d.Verifier.Verify(in.Session)
		if err != nil {
			return nil, authTokenUnauthenticated(jwt.ClassifyVerifyErr(err))
		}
		// 4. Revoked check against the in-memory RBAC snapshot (map-lookup): a
		// revoked Archon cannot exchange an otherwise-live cookie for a new Bearer.
		if d.Revoked != nil && d.Revoked.IsRevoked(claims.Subject) {
			return nil, authTokenUnauthenticated("")
		}
		// 5. TTL cap on cookie.exp: the issued Bearer cannot outlive the source session.
		ttl := d.TTL
		if rem := time.Until(claims.ExpiresAt); rem < ttl {
			ttl = rem
		}
		// defensive guard for a future leeway: currently UNREACHABLE — Verify
		// without leeway already rejected an expired cookie (ErrExpiredToken) at
		// step 3, so claims.ExpiresAt is strictly in the future and rem>0. This
		// branch would only activate if clock-leeway is added to Verify (then a
		// cookie with exp in the past by ≤leeway would pass, and rem could become
		// ≤0). Explicit 401 instead of Issue(ttl≤0)→error/500.
		if ttl <= 0 {
			return nil, authTokenUnauthenticated("")
		}
		// 6. Issue. Subject/roles/bootstrapInitial — STRICTLY from verified claims
		// (no privilege-escalation via body/query/headers).
		token, err := d.Issuer.Issue(claims.Subject, claims.Roles, ttl, claims.BootstrapInitial)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/token exchange: issue JWT failed",
					slog.String("aid", claims.Subject), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}
		// 7. expires_at — from the ACTUALLY issued token (self-verify), NOT
		// time.Now().Add(ttl): Issue takes its own time.Now() and truncates exp to
		// a second (jwt/v5), so a recomputation would give an exp ≤~1s LATER than
		// real — the client would schedule re-exchange by the wrong deadline and
		// hit a 401 on /v1 in a narrow window. Verify also acts as a self-check:
		// never hand out a token we don't verify ourselves (HS256, microseconds).
		verified, err := d.Verifier.Verify(token)
		if err != nil {
			if d.Logger != nil {
				d.Logger.Error("auth/token exchange: self-verify of the issued token failed",
					slog.String("aid", claims.Subject), slog.Any("error", err))
			}
			return nil, huma.NewError(http.StatusInternalServerError, "internal error")
		}
		return &authTokenOutput{Body: AuthTokenReply{Token: token, ExpiresAt: verified.ExpiresAt}}, nil
	})
}

// registerHumaAuthMethods mounts the public GET /auth/methods. NOT opt-in
// (methods are always declared): value-deps, no no-op branch.
func registerHumaAuthMethods(humaAPI huma.API, d AuthMethodsDeps) {
	huma.Register(humaAPI, authMethodsOperation(), func(_ context.Context, _ *authMethodsInput) (*authMethodsOutput, error) {
		return &authMethodsOutput{Body: AuthMethodsReply{Password: d.Password, LDAP: d.LDAP, OIDC: d.OIDC}}, nil
	})
}

// authTokenUnauthenticated — generic 401 problem+json. An empty detail means a
// neutral message (anti-oracle: we don't distinguish "no cookie"/"revoked"/
// "ttl expired"); a non-empty detail comes only from [jwt.ClassifyVerifyErr]
// (public-safe, the same set RequireJWT already returns for Bearer — no new
// oracle surface).
func authTokenUnauthenticated(detail string) huma.StatusError {
	if detail == "" {
		detail = "authentication required"
	}
	return humaProblemError{Details: problemWithStatus(problem.TypeUnauthenticated, http.StatusUnauthorized, detail)}
}

// authTokenForbidden — 403 for an explicit cross-site exchange (sanitized).
func authTokenForbidden() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "cross-site token exchange rejected")}
}

// authTokenSpecStub — a non-nil dependency stub for the spec dump (the handler
// is not called during dump, non-nil is only needed to register the operation).
func authTokenSpecStub() *AuthTokenDeps { return &AuthTokenDeps{} }

// authMethodsSpecStub — a value stub for the spec dump.
func authMethodsSpecStub() AuthMethodsDeps { return AuthMethodsDeps{} }
