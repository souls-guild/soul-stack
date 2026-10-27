package api

// Local-password login: POST /auth/local/login (NIM-1051). The same shape as
// the LDAP login (huma_auth.go) — public, outside /v1, under AuthLoginLimit —
// with the password checked by Keeper itself: Login (the check, then an
// `operator_jwts` row of kind `session` bound to it) → the same `soul_session`
// cookie the SPA exchanges at POST /auth/token. No mapping step: the operator
// already exists with `auth_method = local`.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// LocalAuthDeps — dependencies of the local-password login. When
// [Deps.LocalAuth] is nil the endpoint is not mounted (`auth.local` absent).
type LocalAuthDeps struct {
	Authenticator localAuthenticator
	TTL           time.Duration
	// InsecureCookies is keeper.yml auth.insecure_cookies (see newSessionCookie).
	InsecureCookies bool
	Audit           audit.Writer
	Logger          *slog.Logger
}

// localAuthenticator — the narrow contract of *local.Authenticator. Unlike the
// LDAP path it mints the session itself: the mint has to be bound to the
// password that verified, in one transaction with a lock on its row.
type localAuthenticator interface {
	Login(ctx context.Context, aid, password string, ttl time.Duration, client operator.ClientMeta) (auth.MappedOperator, string, error)
}

type localLoginInput struct {
	Body LocalLoginRequest
}

// LocalLoginRequest — the body of POST /auth/local/login. The password is
// never logged and never put into audit. maxLength bounds what is hashed: a
// set password is at most 128 characters.
type LocalLoginRequest struct {
	AID      string `json:"aid" minLength:"1" maxLength:"128" doc:"the operator's AID"`
	Password string `json:"password" format:"password" minLength:"1" maxLength:"512" doc:"local password (never logged, never returned)"`
}

type localLoginOutput struct {
	Status    int    `json:"-"`
	SetCookie string `header:"Set-Cookie" json:"-"`
}

func localLoginOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "localLogin",
		Method:        http.MethodPost,
		Path:          "/local/login",
		Summary:       "Operator login with a local password",
		Description:   "NIM-1051: checks the AID's local password (argon2id) and puts an internal JWT into the HttpOnly soul_session cookie (Secure unless keeper.yml auth.insecure_cookies); exchange it at POST /auth/token. Only operators with auth_method=local log in here. Every credential refusal is the same 401, including a password reset that landed between the check and the session. 413 - body over 4 KiB (while the login rate limit runs). Mounted only when auth.local is configured. Response body is empty.",
		Tags:          []string{"auth"},
		DefaultStatus: http.StatusNoContent,
		// 413 — a body over the 4 KiB the per-AID key is read from (AuthLoginLimit).
		Errors: []int{http.StatusUnauthorized, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
}

// registerHumaLocalLogin mounts POST /auth/local/login. d nil → no-op.
func registerHumaLocalLogin(humaAPI huma.API, d *LocalAuthDeps) {
	if d == nil {
		return
	}
	huma.Register(humaAPI, localLoginOperation(), func(ctx context.Context, in *localLoginInput) (*localLoginOutput, error) {
		client := apimiddleware.ClientMetaFromContext(ctx)
		op, token, err := d.Authenticator.Login(ctx, in.Body.AID, in.Body.Password, d.TTL,
			operator.ClientMeta{IP: client.IP, UserAgent: client.UserAgent})
		if err != nil {
			if d.Logger != nil && !isCredentialRefusal(err) {
				d.Logger.Error("auth/local login failed", slog.Any("error", err))
			}
			return nil, ldapLoginProblem(err)
		}
		cookie := newSessionCookie(token, d.TTL, d.InsecureCookies)
		if d.Audit != nil {
			ev := &audit.Event{
				AuditID:   audit.NewULID(),
				EventType: audit.EventOperatorLogin,
				Source:    audit.SourceAPI,
				ArchonAID: op.AID,
				Payload: map[string]any{
					"method":      "local",
					"aid":         op.AID,
					"provisioned": false,
				},
			}
			if err := d.Audit.Write(ctx, ev); err != nil && d.Logger != nil {
				d.Logger.Error("auth/local login: audit write failed (login succeeded)",
					slog.String("aid", op.AID), slog.Any("error", err))
			}
		}
		return &localLoginOutput{Status: http.StatusNoContent, SetCookie: cookie.String()}, nil
	})
}

func isCredentialRefusal(err error) bool {
	return errors.Is(err, auth.ErrAuthFailed) || errors.Is(err, auth.ErrOperatorRevoked)
}

// localAuthSpecStub — a non-nil dependency stub for the spec dump.
func localAuthSpecStub() *LocalAuthDeps { return &LocalAuthDeps{} }
