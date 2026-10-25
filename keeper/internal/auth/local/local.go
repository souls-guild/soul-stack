// Package local — the local-password login method (NIM-1051): Keeper checks an
// operator's password itself, against the argon2id hash in
// `operator_passwords`, instead of asking an external identity provider.
//
// Unlike LDAP/OIDC there is no mapping step: the operator must already exist
// with `auth_method = local`, and its roles are whatever RBAC holds for it.
// The anti-takeover rule of ADR-058(d) holds the same way — a password logs in
// only an operator enrolled by this method.
package local

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// RBACSource gives the roles the session token carries. They are informational:
// every request is authorized against the RBAC snapshot (ADR-028).
type RBACSource interface {
	RolesOf(aid string) []string
}

// maxPasswordBytes bounds what is hashed at login. A set password is at most
// [operator.MaxPasswordLength] characters, 4 bytes each at worst.
const maxPasswordBytes = 4 * operator.MaxPasswordLength

// Pool is what the login reads the hash from and mints the session in.
// `*pgxpool.Pool` satisfies it.
type Pool interface {
	operator.ExecQueryRower
	operator.TxBeginner
}

// Authenticator checks a local password and mints the session it earns.
type Authenticator struct {
	db     Pool
	rbac   RBACSource
	issuer operator.JWTIssuer
	logger *slog.Logger
}

// New builds an Authenticator.
func New(db Pool, rbac RBACSource, issuer operator.JWTIssuer, logger *slog.Logger) (*Authenticator, error) {
	if db == nil {
		return nil, errors.New("auth/local: db is nil")
	}
	if rbac == nil {
		return nil, errors.New("auth/local: rbac is nil")
	}
	if issuer == nil {
		return nil, errors.New("auth/local: issuer is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{db: db, rbac: rbac, issuer: issuer, logger: logger}, nil
}

// Method implements [auth.Authenticator].
func (a *Authenticator) Method() string { return string(operator.AuthMethodLocal) }

// Login checks that password is aid's local password and mints the session
// it earns: an `operator_jwts` row of kind `session` with ttl, and the token.
// Every credential refusal is [auth.ErrAuthFailed] — an unknown AID, an
// operator of another method, a wrong password, and a password reset between
// the check and the mint are not told apart (anti-oracle).
// [auth.ErrOperatorRevoked] is returned only after the password matched.
func (a *Authenticator) Login(ctx context.Context, aid, password string, ttl time.Duration, client operator.ClientMeta) (auth.MappedOperator, string, error) {
	aid = operator.NormalizeLoginAID(aid)
	if !operator.ValidAID(aid) || password == "" || len(password) > maxPasswordBytes {
		return auth.MappedOperator{}, "", auth.ErrAuthFailed
	}
	check, err := operator.VerifyLocalPassword(ctx, a.db, aid, password)
	switch {
	case err == nil:
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return auth.MappedOperator{}, "", auth.ErrOperatorRevoked
	case errors.Is(err, operator.ErrNotLocalOperator):
		a.logger.Warn("auth/local: password login refused — the AID does not log in with a local password",
			slog.String("aid", aid))
		return auth.MappedOperator{}, "", auth.ErrAuthFailed
	case errors.Is(err, operator.ErrOperatorNotFound), errors.Is(err, operator.ErrPasswordMismatch):
		return auth.MappedOperator{}, "", auth.ErrAuthFailed
	default:
		return auth.MappedOperator{}, "", err
	}
	op := auth.MappedOperator{AID: aid, Roles: a.rbac.RolesOf(aid)}
	minted, err := operator.MintPasswordSession(ctx, a.db, a.issuer, check, operator.MintInput{
		AID: aid, Roles: op.Roles, TTL: ttl, Client: client,
	})
	switch {
	case errors.Is(err, operator.ErrPasswordMismatch):
		return auth.MappedOperator{}, "", auth.ErrAuthFailed
	case err != nil:
		return auth.MappedOperator{}, "", err
	}
	return op, minted.JWT, nil
}
