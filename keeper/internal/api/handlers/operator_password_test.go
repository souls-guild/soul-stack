package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// fixtureHash is argon2id (m=1024,t=1,p=1) of "correct horse battery".
const fixtureHash = "$argon2id$v=19$m=1024,t=1,p=1$Zml4dHVyZS1zYWx0LTE2Yg$hZjA5/pFEJk4YrEB5a/UEW68i7vKebQm9e1yPJjI4VY"

func passwordHandler(t *testing.T, pool *fakePool, policy bool) *OperatorHandler {
	t.Helper()
	h, _ := newHandler(t, pool, &rbactest.Config{})
	if policy {
		h.Service().SetPasswordPolicy(&operator.PasswordPolicy{MinLength: 12})
	}
	return h
}

func localOperator(aid string) func(string) (*operator.Operator, error) {
	return func(string) (*operator.Operator, error) {
		return &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodLocal, CreatedAt: time.Unix(0, 0)}, nil
	}
}

// A local Archon is created with its password and without a token: no
// operator_jwts row, no JWT in the reply, the hash written by the creator.
func TestOperatorCreateTyped_LocalGetsAPasswordAndNoToken(t *testing.T) {
	pool := &fakePool{selectFn: localOperator("alice")}
	h := passwordHandler(t, pool, true)

	reply, err := h.CreateTyped(context.Background(), claims("archon-root"), OperatorCreateInput{
		AID: "alice", AuthMethod: "local", Password: "correct horse battery",
	})
	if err != nil {
		t.Fatalf("CreateTyped local: %v", err)
	}
	if reply.AuthMethod != "local" || reply.JWT != "" || reply.JWTID != "" || !reply.ExpiresAt.IsZero() {
		t.Fatalf("reply = %+v, want auth_method local and no token", reply)
	}
	if len(pool.jwtInserts) != 0 {
		t.Fatalf("operator_jwts inserts = %d, want 0", len(pool.jwtInserts))
	}
	if len(pool.passwordUpserts) != 1 {
		t.Fatalf("password upserts = %d, want 1", len(pool.passwordUpserts))
	}
	args := pool.passwordUpserts[0]
	if args[0] != "alice" || !strings.HasPrefix(args[1].(string), "$argon2id$") || args[2] != "archon-root" {
		t.Fatalf("password upsert = %v, want alice, an argon2id hash, set by archon-root", args)
	}
	if strings.Contains(args[1].(string), "correct horse battery") {
		t.Fatal("the password is stored in clear")
	}
	if _, ok := reply.AuditPayload()["jwt_id"]; ok {
		t.Fatal("audit payload names a jwt_id for an operator created without a token")
	}
}

// The provisioning policy gates each kind of creation by its own method: `user`
// for a token Archon, `local` for a password one (NIM-1051).
func TestOperatorCreateTyped_PolicyGatesLocalByItsOwnMethod(t *testing.T) {
	pool := &fakePool{selectFn: localOperator("alice")}
	h := passwordHandler(t, pool, true)

	h.SetProvisioningGate(fakeProvisioningGate{allowed: map[string]bool{"user": true}})
	_, err := h.CreateTyped(context.Background(), claims("archon-root"), OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "correct horse battery"})
	wantProblem(t, err, problem.TypeProvisioningMethodDisabled)
	if pool.insertCalls != 0 {
		t.Fatal("a refused local create inserted an operator")
	}

	h.SetProvisioningGate(fakeProvisioningGate{allowed: map[string]bool{"local": true}})
	_, err = h.CreateTyped(context.Background(), claims("archon-root"), OperatorCreateInput{AID: "bob"})
	wantProblem(t, err, problem.TypeProvisioningMethodDisabled)
	if _, err := h.CreateTyped(context.Background(), claims("archon-root"), OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "correct horse battery"}); err != nil {
		t.Fatalf("local create with local allowed: %v", err)
	}
}

func TestOperatorCreateTyped_LocalRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		policy bool
		in     OperatorCreateInput
	}{
		{"auth.local not configured", false, OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "correct horse battery"}},
		{"weak password", true, OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "short"}},
		{"password equal to the AID", true, OperatorCreateInput{AID: "alice-the-operator", AuthMethod: "local", Password: "ALICE-THE-OPERATOR"}},
		{"token terms for a local operator", true, OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "correct horse battery", TTL: "90d"}},
		{"a label for a local operator", true, OperatorCreateInput{AID: "alice", AuthMethod: "local", Password: "correct horse battery", Label: "laptop"}},
		{"a password for a token operator", true, OperatorCreateInput{AID: "alice", Password: "correct horse battery"}},
		{"a method create does not take", true, OperatorCreateInput{AID: "alice", AuthMethod: "ldap"}},
	} {
		pool := &fakePool{selectFn: localOperator("alice")}
		h := passwordHandler(t, pool, c.policy)
		_, err := h.CreateTyped(context.Background(), claims("archon-root"), c.in)
		d, ok := AsProblemDetails(err)
		if !ok || d.Type != problem.TypeValidationFailed {
			t.Errorf("%s: %v, want validation-failed", c.name, err)
		}
		if pool.insertCalls != 0 || len(pool.passwordUpserts) != 0 {
			t.Errorf("%s: wrote before refusing", c.name)
		}
	}
}

func TestOperatorSetPasswordTyped(t *testing.T) {
	revoked := time.Unix(1, 0)
	for _, c := range []struct {
		name   string
		target func(string) (*operator.Operator, error)
		policy bool
		pw     string
		want   string
	}{
		{"jwt operator", activeOperatorSelect(operator.AuthMethodJWT, nil), true, "a brand new password", problem.TypeValidationFailed},
		{"revoked", activeOperatorSelect(operator.AuthMethodLocal, &revoked), true, "a brand new password", problem.TypeOperatorRevoked},
		{"unknown", nil, true, "a brand new password", problem.TypeNotFound},
		{"weak", activeOperatorSelect(operator.AuthMethodLocal, nil), true, "short", problem.TypeValidationFailed},
		{"auth.local not configured", activeOperatorSelect(operator.AuthMethodLocal, nil), false, "a brand new password", problem.TypeValidationFailed},
	} {
		pool := &fakePool{selectFn: c.target}
		h := passwordHandler(t, pool, c.policy)
		_, err := h.SetPasswordTyped(context.Background(), claims("archon-root"), "alice", c.pw)
		wantProblem(t, err, c.want)
		if len(pool.passwordUpserts) != 0 || len(pool.tokenRevokes) != 0 {
			t.Errorf("%s: wrote before refusing", c.name)
		}
	}

	pool := &fakePool{selectFn: activeOperatorSelect(operator.AuthMethodLocal, nil), tokensRevoked: 3}
	h := passwordHandler(t, pool, true)
	reply, err := h.SetPasswordTyped(context.Background(), claims("archon-root"), "alice", "a brand new password")
	if err != nil {
		t.Fatalf("SetPasswordTyped: %v", err)
	}
	if reply.TokensRevoked != 3 || reply.AuditPayload()["tokens_revoked"] != int64(3) {
		t.Fatalf("reply = %+v, want 3 tokens revoked in the audit payload", reply)
	}
	if len(pool.tokenRevokes) != 1 || pool.tokenRevokes[0][2] != "" {
		t.Fatalf("token revocation = %v, want one revoking every token (no kept jti)", pool.tokenRevokes)
	}
}

// The administrative route asks for no current password, so it refuses the
// caller's own AID: a stolen token holding operator.set-password must not buy
// its owner's password (NIM-1051). PUT /v1/me/password is the way.
func TestOperatorSetPasswordTyped_RefusesTheCallersOwnAID(t *testing.T) {
	pool := &fakePool{selectFn: activeOperatorSelect(operator.AuthMethodLocal, nil)}
	h := passwordHandler(t, pool, true)
	_, err := h.SetPasswordTyped(context.Background(), claims("alice"), "alice", "a brand new password")
	wantProblem(t, err, problem.TypeValidationFailed)
	if len(pool.passwordUpserts) != 0 || len(pool.tokenRevokes) != 0 {
		t.Fatal("a self-reset through the administrative route wrote")
	}
}

func activeOperatorSelect(m operator.AuthMethod, revokedAt *time.Time) func(string) (*operator.Operator, error) {
	return func(aid string) (*operator.Operator, error) {
		return &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: m, RevokedAt: revokedAt}, nil
	}
}

func TestOperatorChangeOwnPasswordTyped(t *testing.T) {
	hash := fixtureHash
	lookup := func(string) (string, bool, *string, bool) { return "local", false, &hash, true }
	me := &keeperjwt.Claims{Subject: "alice", ID: "01JTESTJT1000000000000000A"}

	pool := &fakePool{passwordFn: lookup}
	h := passwordHandler(t, pool, true)
	_, err := h.ChangeOwnPasswordTyped(context.Background(), me, "not the password", "a brand new password")
	// 403, not 401: a 401 would log the caller out of a session that is fine.
	wantProblem(t, err, problem.TypeForbidden)
	if len(pool.passwordUpserts) != 0 {
		t.Fatal("a wrong current password changed the password")
	}

	pool = &fakePool{passwordFn: func(string) (string, bool, *string, bool) { return "jwt", false, nil, true }}
	h = passwordHandler(t, pool, true)
	_, err = h.ChangeOwnPasswordTyped(context.Background(), me, "correct horse battery", "a brand new password")
	wantProblem(t, err, problem.TypeValidationFailed)

	pool = &fakePool{passwordFn: lookup, tokensRevoked: 1}
	h = passwordHandler(t, pool, true)
	reply, err := h.ChangeOwnPasswordTyped(context.Background(), me, "correct horse battery", "a brand new password")
	if err != nil {
		t.Fatalf("ChangeOwnPasswordTyped: %v", err)
	}
	if reply.AID != "alice" || reply.TokensRevoked != 1 {
		t.Fatalf("reply = %+v, want alice with 1 session revoked", reply)
	}
	if len(pool.passwordUpserts) != 1 || pool.passwordUpserts[0][2] != "alice" {
		t.Fatalf("password upsert = %v, want one set by alice herself", pool.passwordUpserts)
	}
	if len(pool.tokenRevokes) != 1 || pool.tokenRevokes[0][2] != me.ID {
		t.Fatalf("session revocation = %v, want the caller's own jti %s kept", pool.tokenRevokes, me.ID)
	}
}

// The change is bound to the password that verified: a reset committed between
// the check and the write refuses it (403) rather than being overwritten.
func TestOperatorChangeOwnPasswordTyped_RefusedWhenResetMeanwhile(t *testing.T) {
	hash, reset := fixtureHash, "$argon2id$v=19$m=1024,t=1,p=1$b3RoZXItc2FsdA$aGFzaA"
	pool := &fakePool{
		passwordFn: func(string) (string, bool, *string, bool) { return "local", false, &hash, true },
		rereadHash: &reset,
	}
	h := passwordHandler(t, pool, true)
	_, err := h.ChangeOwnPasswordTyped(context.Background(), &keeperjwt.Claims{Subject: "alice"}, "correct horse battery", "a brand new password")
	wantProblem(t, err, problem.TypeForbidden)
	if len(pool.passwordUpserts) != 0 {
		t.Fatal("a change bound to a stale check overwrote the reset")
	}
}

// A session a reset revoked can still verify on a node whose snapshot lags; a
// personal token asked for with it is the identity verdict 401, not a 500.
func TestIssuePersonalJWTTyped_FromARevokedSessionIs401(t *testing.T) {
	pool := &fakePool{callerJWTRevoked: true}
	h := passwordHandler(t, pool, true)
	_, err := h.IssuePersonalJWTTyped(context.Background(), &keeperjwt.Claims{Subject: "alice", ID: "01JTESTJT1000000000000000A"}, OperatorIssueTokenInput{})
	wantProblem(t, err, problem.TypeJWTRevoked)
	if len(pool.jwtInserts) != 0 {
		t.Fatal("a personal token was minted from a revoked session")
	}
}

// Revoked between RejectRevoked and the check: the identity verdict, not a 500.
func TestOperatorChangeOwnPasswordTyped_RevokedMeanwhileIs401(t *testing.T) {
	hash := fixtureHash
	pool := &fakePool{passwordFn: func(string) (string, bool, *string, bool) { return "local", true, &hash, true }}
	h := passwordHandler(t, pool, true)
	_, err := h.ChangeOwnPasswordTyped(context.Background(), &keeperjwt.Claims{Subject: "alice"}, "correct horse battery", "a brand new password")
	wantProblem(t, err, problem.TypeOperatorRevokedToken)
}
