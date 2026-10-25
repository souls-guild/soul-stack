//go:build integration

// Integration tests for local passwords (migration 130, NIM-1051) against real
// Postgres: the composite foreign key that keeps a hash off any operator whose
// method is not `local`, the upsert, and the session revocation a password
// change carries. The unit tests drive VerifyLocalPassword through a fake row,
// which proves the order of the checks and not one line of the SQL.

package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newPasswordService(t *testing.T) *Service {
	t.Helper()
	fastHashes(t)
	s := newIntegrationService(t)
	s.SetPasswordPolicy(&PasswordPolicy{MinLength: 12})
	return s
}

func revokedAt(t *testing.T, jwtID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM operator_jwts WHERE jwt_id = $1`, jwtID).Scan(&at); err != nil {
		t.Fatalf("read %s: %v", jwtID, err)
	}
	return at
}

func TestIntegration_LocalPassword_CreateAndVerify(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)

	res, err := s.Create(ctx, CreateInput{
		AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal,
		Password: "correct horse battery", Roles: []string{"cluster-admin"},
	})
	if err != nil {
		t.Fatalf("Create local: %v", err)
	}
	if res.AuthMethod != AuthMethodLocal || res.JWT != "" || res.JWTID != "" || !res.ExpiresAt.IsZero() {
		t.Fatalf("Create local = %+v, want auth_method local and no token", res)
	}
	var tokens int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operator_jwts WHERE aid = 'alice'`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("operator_jwts rows for a local operator = %d (%v), want 0", tokens, err)
	}
	var setBy *string
	if err := integrationPool.QueryRow(ctx, `SELECT set_by_aid FROM operator_passwords WHERE aid = 'alice'`).Scan(&setBy); err != nil || setBy == nil || *setBy != "archon-root" {
		t.Fatalf("operator_passwords.set_by_aid = %v (%v), want archon-root", setBy, err)
	}

	if _, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery"); err != nil {
		t.Fatalf("verify the right password: %v", err)
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse batterY"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("verify a wrong password: %v, want ErrPasswordMismatch", err)
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "archon-root", "correct horse battery"); !errors.Is(err, ErrNotLocalOperator) {
		t.Fatalf("verify a jwt operator: %v, want ErrNotLocalOperator", err)
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "nobody", "correct horse battery"); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("verify an unknown AID: %v, want ErrOperatorNotFound", err)
	}
}

// The schema keeps the anti-takeover rule by itself: a hash row cannot point at
// an operator whose method is not `local`, whatever the Go code does.
func TestIntegration_LocalPassword_SchemaRefusesAHashForAJWTOperator(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedActiveOperator(t, "archon-root", nil)
	fastHashes(t)
	h, err := hashPassword(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// The bare sentinel, not one wrapping the driver's error: its text reaches
	// a 422 detail.
	if err := storePasswordHash(ctx, integrationPool, "archon-root", h, ""); err != ErrNotLocalOperator {
		t.Fatalf("store a hash for a jwt operator: %v, want ErrNotLocalOperator from the composite foreign key", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operator_passwords (aid, auth_method, hash) VALUES ('archon-root', 'jwt', $1)`, h); err == nil {
		t.Fatal("a row with auth_method jwt was accepted; the CHECK operator_passwords_local_only is gone")
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operators (aid, display_name, auth_method) VALUES ('bob', 'bob', 'local')`); err != nil {
		t.Fatalf("auth_method local refused by the operators CHECK: %v", err)
	}
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO operator_passwords (aid, hash) VALUES ('bob', 'plaintext')`); err == nil {
		t.Fatal("a non-argon2id hash was accepted; the CHECK operator_passwords_hash_format is gone")
	}
}

func TestIntegration_LocalPassword_ResetRevokesEverySession(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	laptop := mintFor(t, "alice", JWTKindSession, "", "")
	phone := mintFor(t, "alice", JWTKindSession, "", "")
	script := mintFor(t, "alice", JWTKindIssued, "archon-root", "script")
	// A personal token alice — or whoever had her password — issued herself.
	own := mintFor(t, "alice", JWTKindPersonal, "alice", "mine")

	n, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-root"})
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if n != 3 {
		t.Fatalf("tokens revoked = %d, want 2 sessions and 1 personal", n)
	}
	if revokedAt(t, own.JWTID) == nil {
		t.Fatal("a personal token survived the reset; whoever held the old password could keep it for ttl_max")
	}
	if revokedAt(t, laptop.JWTID) == nil || revokedAt(t, phone.JWTID) == nil {
		t.Fatal("a session survived the reset")
	}
	if revokedAt(t, script.JWTID) != nil {
		t.Fatal("an issued token was revoked by a password reset; only sessions belong to the password")
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("the old password after a reset: %v, want ErrPasswordMismatch", err)
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "alice", "a brand new password"); err != nil {
		t.Fatalf("the new password after a reset: %v", err)
	}

	seedActiveOperator(t, "bob", nil)
	if _, err := s.SetPassword(ctx, SetPasswordInput{AID: "bob", Password: "a brand new password", CallerAID: "archon-root"}); !errors.Is(err, ErrNotLocalOperator) {
		t.Fatalf("SetPassword on a jwt operator: %v, want ErrNotLocalOperator", err)
	}
	if _, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "the third password", CallerAID: "alice"}); !errors.Is(err, ErrOwnPasswordViaAdmin) {
		t.Fatalf("SetPassword on the caller's own AID: %v, want ErrOwnPasswordViaAdmin", err)
	}
}

func TestIntegration_LocalPassword_ChangeKeepsTheCurrentSession(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	current := mintFor(t, "alice", JWTKindSession, "", "")
	other := mintFor(t, "alice", JWTKindSession, "", "")
	script := mintFor(t, "alice", JWTKindPersonal, "alice", "my script")

	if _, err := s.ChangePassword(ctx, ChangePasswordInput{AID: "alice", Current: "not the password", New: "a brand new password", CurrentJWTID: current.JWTID}); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("ChangePassword with a wrong current password: %v, want ErrPasswordMismatch", err)
	}
	if revokedAt(t, other.JWTID) != nil {
		t.Fatal("a refused change revoked a session")
	}

	n, err := s.ChangePassword(ctx, ChangePasswordInput{AID: "alice", Current: "correct horse battery", New: "a brand new password", CurrentJWTID: current.JWTID})
	if err != nil || n != 1 {
		t.Fatalf("ChangePassword = %d, %v; want 1 session revoked", n, err)
	}
	if revokedAt(t, current.JWTID) != nil {
		t.Fatal("the session the change came from was revoked")
	}
	if revokedAt(t, other.JWTID) == nil {
		t.Fatal("another session survived the change")
	}
	if revokedAt(t, script.JWTID) != nil {
		t.Fatal("a change by the operator itself revoked its personal token")
	}
	var setBy *string
	if err := integrationPool.QueryRow(ctx, `SELECT set_by_aid FROM operator_passwords WHERE aid = 'alice'`).Scan(&setBy); err != nil || setBy == nil || *setBy != "alice" {
		t.Fatalf("set_by_aid after a self change = %v (%v), want alice", setBy, err)
	}
}

func TestIntegration_LocalPassword_OffWithoutPolicy(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newIntegrationService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); !errors.Is(err, ErrLocalPasswordsDisabled) {
		t.Fatalf("Create local without auth.local: %v, want ErrLocalPasswordsDisabled", err)
	}
	if _, err := SelectByAID(ctx, integrationPool, "alice"); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("a refused create left an operator behind: %v", err)
	}
}

// A login whose password verified before a reset committed gets no session:
// the mint re-reads the hash under lock and refuses (validator finding, NIM-1051).
func TestIntegration_LocalPassword_LoginAfterAResetMintsNothing(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	check, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-root"}); err != nil {
		t.Fatal(err)
	}
	if _, err := MintPasswordSession(ctx, integrationPool, &fakeIssuer{}, check, MintInput{AID: "alice", TTL: time.Hour}); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("mint after a reset: %v, want ErrPasswordMismatch", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operator_jwts WHERE aid = 'alice'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("operator_jwts rows for alice = %d (%v), want 0", n, err)
	}
}

// A reset still running when the login mints: the mint waits for it on the row
// lock and then refuses, rather than slipping a session in under the reset's
// revocation.
func TestIntegration_LocalPassword_MintWaitsForARunningReset(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	check, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := hashPassword(ctx, "a brand new password")
	if err != nil {
		t.Fatal(err)
	}
	reset, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reset.Rollback(ctx) }()
	if err := storePasswordHash(ctx, reset, "alice", newHash, "archon-root"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := MintPasswordSession(ctx, integrationPool, &fakeIssuer{}, check, MintInput{AID: "alice", TTL: time.Hour})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the mint did not wait for the running reset: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := reset.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrPasswordMismatch) {
			t.Fatalf("mint after the reset committed: %v, want ErrPasswordMismatch", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the mint never returned after the reset committed")
	}
}

// A change whose current password verified before a reset committed does not
// overwrite the reset.
func TestIntegration_LocalPassword_ChangeDoesNotUndoAReset(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	check, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "the administrator's one", CallerAID: "archon-root"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.replacePassword(ctx, "alice", "the attacker's choice", "alice", "", &check, changeRevokesKinds); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("change bound to a pre-reset check: %v, want ErrPasswordMismatch", err)
	}
	if _, err := VerifyLocalPassword(ctx, integrationPool, "alice", "the administrator's one"); err != nil {
		t.Fatalf("the reset was overwritten: %v", err)
	}
}

// A session that expired a minute ago may still verify on a Keeper whose clock
// lags; the revocation covers the RBAC snapshot's five-minute window.
func TestIntegration_RevokeTokensOfKinds_CoversTheSnapshotWindow(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	for id, expired := range map[string]string{
		"01JTESTSESSN00000000000001": "1 minute",
		"01JTESTSESSN00000000000002": "10 minutes",
	} {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO operator_jwts (jwt_id, aid, kind, created_at, expires_at)
			 VALUES ($1, 'alice', 'session', NOW() - INTERVAL '1 day', NOW() - $2::interval)`, id, expired); err != nil {
			t.Fatal(err)
		}
	}
	n, err := RevokeTokensOfKinds(ctx, integrationPool, "alice", "", "archon-root", changeRevokesKinds)
	if err != nil || n != 1 {
		t.Fatalf("RevokeTokensOfKinds = %d, %v; want 1 (the one inside the window)", n, err)
	}
	if revokedAt(t, "01JTESTSESSN00000000000001") == nil {
		t.Fatal("a session expired a minute ago was not revoked")
	}
}

// blockingIssuer holds MintJWT inside its transaction — after the session row
// is written, before the commit — until release is closed.
type blockingIssuer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// unblock releases the mint. A test registers it as a cleanup too: failing
// before the release would leave the mint holding its connection, and closing
// the pool would wait for it forever.
func (b *blockingIssuer) unblock() { b.once.Do(func() { close(b.release) }) }

// waitEntered waits for the mint to reach the issuer. A mint that fails before
// it never does, so the wait is bounded instead of hanging the test.
func (b *blockingIssuer) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the mint never reached the issuer")
	}
}

func (b *blockingIssuer) Issue(id, aid string, _ []string, _ time.Duration, _ bool) (string, error) {
	close(b.entered)
	<-b.release
	return "jwt-" + aid + "-" + id, nil
}

// A reset that starts while a password login holds its row lock waits for the
// login and then revokes the session it minted. This pins the order of the
// reset — upsert, then revoke: revoking first would run before the session row
// is visible, and the session would survive the reset.
func TestIntegration_LocalPassword_ResetDuringAMintRevokesItsSession(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	check, err := VerifyLocalPassword(ctx, integrationPool, "alice", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	iss := &blockingIssuer{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(iss.unblock)
	minted := make(chan MintResult, 1)
	go func() {
		res, err := MintPasswordSession(ctx, integrationPool, iss, check, MintInput{AID: "alice", TTL: time.Hour})
		if err != nil {
			t.Errorf("mint: %v", err)
		}
		minted <- res
	}()
	iss.waitEntered(t)

	reset := make(chan int64, 1)
	go func() {
		n, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-root"})
		if err != nil {
			t.Errorf("reset: %v", err)
		}
		reset <- n
	}()
	select {
	case n := <-reset:
		t.Fatalf("the reset finished (%d revoked) while the login held its row lock", n)
	case <-time.After(300 * time.Millisecond):
	}
	iss.unblock()

	res := <-minted
	if n := <-reset; n != 1 {
		t.Fatalf("the reset revoked %d sessions, want the 1 the login minted", n)
	}
	if revokedAt(t, res.JWTID) == nil {
		t.Fatal("the session minted with the old password survived the reset")
	}
}

// A session a reset revoked may still verify on a node whose RBAC snapshot has
// not heard of it; minting a personal token from it would outlive the reset.
// The issue reads the revocation from the row.
func TestIntegration_LocalPassword_PersonalTokenFromASessionTheResetRevokedIsRefused(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	session := sessionOf(t, "alice")
	if _, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-root"}); err != nil {
		t.Fatal(err)
	}
	if _, err := issuePersonal(s, "alice", session, time.Hour, ""); !errors.Is(err, ErrCallerJWTRevoked) {
		t.Fatalf("personal token from a revoked session: err = %v, want ErrCallerJWTRevoked", err)
	}
}

// An issue that starts while a reset holds the lock waits for it and only then
// reads its caller token: it finds the session the reset revoked. Reading the
// token before the lock would see the revocation uncommitted, and the token
// minted after the wait would outlive the reset.
func TestIntegration_LocalPassword_PersonalIssueDuringAResetIsRefused(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	session := sessionOf(t, "alice")

	// A reset in flight: the lock taken and the session revoked, not committed.
	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := RevokeTokensOfKinds(ctx, tx, "alice", "", "archon-root", resetRevokesKinds); err != nil {
		t.Fatal(err)
	}

	issued := make(chan error, 1)
	go func() {
		_, err := issuePersonal(s, "alice", session, time.Hour, "")
		issued <- err
	}()
	// The issue must be waiting on the lock, not merely late.
	waitForALockWait(t)
	select {
	case err := <-issued:
		t.Fatalf("the issue finished (%v) while a reset held the lock", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-issued; !errors.Is(err, ErrCallerJWTRevoked) {
		t.Fatalf("personal token issued during a reset: err = %v, want ErrCallerJWTRevoked", err)
	}
}

// A reset that starts while a personal token is being minted waits for it and
// then revokes it: both take lockOperatorTokens, so the revocation cannot run
// before the new row is visible and leave it alive.
func TestIntegration_LocalPassword_ResetDuringAPersonalIssueRevokesTheToken(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	seedClusterAdmin(t, "archon-root", nil)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: "archon-root", AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	session := sessionOf(t, "alice")
	iss := &blockingIssuer{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(iss.unblock)
	issuer, err := NewService(ServiceDeps{
		Pool: integrationPool, Issuer: iss, RBAC: &fakeRBAC{},
		TTLDefault: 24 * time.Hour, TTLMax: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	issued := make(chan *IssueTokenResult, 1)
	go func() {
		res, err := issuePersonal(issuer, "alice", session, time.Hour, "")
		if err != nil {
			t.Errorf("issue: %v", err)
		}
		issued <- res
	}()
	iss.waitEntered(t)

	reset := make(chan int64, 1)
	go func() {
		n, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-root"})
		if err != nil {
			t.Errorf("reset: %v", err)
		}
		reset <- n
	}()
	select {
	case n := <-reset:
		t.Fatalf("the reset finished (%d revoked) while the issue held the lock", n)
	case <-time.After(300 * time.Millisecond):
	}
	iss.unblock()

	res := <-issued
	if n := <-reset; n != 2 {
		t.Fatalf("the reset revoked %d tokens, want 2: the session and the personal token minted from it", n)
	}
	if res == nil || revokedAt(t, res.JWTID) == nil {
		t.Fatal("the personal token minted during the reset survived it")
	}
}

// waitForALockWait returns once a backend of this database waits on a lock.
func waitForALockWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing waited on a lock")
}

// A reset racing an operator revoke must not deadlock. The revoke holds the
// administrators' rows FOR UPDATE, then updates the target's; the reset's
// writes share-lock the acting administrator's row through their foreign keys.
// A reset that also held the target's row would close the cycle (40P01).
func TestIntegration_LocalPassword_ResetDoesNotDeadlockWithARevoke(t *testing.T) {
	resetOperators(t)
	ctx := context.Background()
	root := "archon-root"
	seedClusterAdmin(t, root, nil)
	seedClusterAdmin(t, "archon-ops", &root)
	s := newPasswordService(t)
	if _, err := s.Create(ctx, CreateInput{AID: "alice", CallerAID: root, AuthMethod: AuthMethodLocal, Password: "correct horse battery"}); err != nil {
		t.Fatal(err)
	}
	sessionOf(t, "alice")

	// The revoke's first half: the administrators' rows, archon-ops among them.
	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM operators WHERE aid IN ('archon-root', 'archon-ops') FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	// archon-ops resets alice: set_by_aid and revoked_by_aid name archon-ops,
	// so the reset waits on the row the revoke holds.
	reset := make(chan error, 1)
	go func() {
		_, err := s.SetPassword(ctx, SetPasswordInput{AID: "alice", Password: "a brand new password", CallerAID: "archon-ops"})
		reset <- err
	}()
	waitForALockWait(t)

	// The revoke's second half: the target's row.
	if _, err := tx.Exec(ctx, `UPDATE operators SET display_name = 'Alice' WHERE aid = 'alice'`); err != nil {
		t.Fatalf("the revoke's update of the reset's target: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-reset; err != nil {
		t.Fatalf("reset: %v", err)
	}
}
