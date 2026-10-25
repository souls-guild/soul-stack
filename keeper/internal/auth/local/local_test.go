package local

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/auth"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// fixtureHash is argon2id (m=1024,t=1,p=1) of "correct horse battery" — cheap
// parameters, read back from the string like any stored hash.
const fixtureHash = "$argon2id$v=19$m=1024,t=1,p=1$Zml4dHVyZS1zYWx0LTE2Yg$hZjA5/pFEJk4YrEB5a/UEW68i7vKebQm9e1yPJjI4VY"

type row struct {
	method  string
	revoked bool
	hash    *string
}

// fakeDB answers the login lookup by AID; an AID it does not hold is no row.
// Its transaction answers the re-read of the hash with currentHash (the hash a
// reset left behind) when set, and records the session rows inserted.
type fakeDB struct {
	rows        map[string]row
	queried     []string
	currentHash *string
	minted      []string
}

func (f *fakeDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) { return &fakeTx{db: f}, nil }

type fakeTx struct {
	pgx.Tx
	db *fakeDB
}

func (t *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "FOR SHARE") {
		return scanFn(func(...any) error { return errors.New("unexpected query in tx") })
	}
	hash := t.db.rows[args[0].(string)].hash
	if t.db.currentHash != nil {
		hash = t.db.currentHash
	}
	return scanFn(func(dest ...any) error {
		*dest[0].(*string) = *hash
		return nil
	})
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "INSERT INTO operator_jwts") {
		return pgconn.CommandTag{}, errors.New("unexpected exec in tx")
	}
	t.db.minted = append(t.db.minted, args[2].(string))
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (t *fakeTx) Commit(context.Context) error   { return nil }
func (t *fakeTx) Rollback(context.Context) error { return nil }

type fakeIssuer struct{}

func (fakeIssuer) Issue(id, aid string, _ []string, _ time.Duration, _ bool) (string, error) {
	return "jwt-" + aid + "-" + id, nil
}

func (f *fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	aid := args[0].(string)
	f.queried = append(f.queried, aid)
	r, ok := f.rows[aid]
	if !ok {
		return scanFn(func(...any) error { return pgx.ErrNoRows })
	}
	return scanFn(func(dest ...any) error {
		*dest[0].(*string) = r.method
		*dest[1].(*bool) = r.revoked
		*dest[2].(**string) = r.hash
		return nil
	})
}

type scanFn func(dest ...any) error

func (f scanFn) Scan(dest ...any) error { return f(dest...) }

type fakeRBAC map[string][]string

func (f fakeRBAC) RolesOf(aid string) []string { return f[aid] }

func newAuth(t *testing.T, db *fakeDB) *Authenticator {
	t.Helper()
	a, err := New(db, fakeRBAC{"alice": {"cluster-admin"}}, fakeIssuer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func login(a *Authenticator, aid, password string) (auth.MappedOperator, string, error) {
	return a.Login(context.Background(), aid, password, time.Hour, operator.ClientMeta{})
}

func TestLogin_LogsInALocalOperatorWithItsRoles(t *testing.T) {
	h := fixtureHash
	db := &fakeDB{rows: map[string]row{"alice": {method: "local", hash: &h}}}
	got, token, err := login(newAuth(t, db), "  Alice ", "correct horse battery")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	want := auth.MappedOperator{AID: "alice", Roles: []string{"cluster-admin"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Login = %+v, want %+v", got, want)
	}
	if len(db.queried) != 1 || db.queried[0] != "alice" {
		t.Fatalf("looked up %v, want the normalized AID alice", db.queried)
	}
	if len(db.minted) != 1 || db.minted[0] != string(operator.JWTKindSession) || !strings.HasPrefix(token, "jwt-alice-") {
		t.Fatalf("minted %v / token %q, want one session row and its token", db.minted, token)
	}
}

// A reset that committed between the check and the mint: the same refusal as a
// wrong password, and no session.
func TestLogin_RefusesWhenThePasswordChangedBeforeTheMint(t *testing.T) {
	h, other := fixtureHash, "$argon2id$v=19$m=1024,t=1,p=1$b3RoZXItc2FsdA$aGFzaA"
	db := &fakeDB{rows: map[string]row{"alice": {method: "local", hash: &h}}, currentHash: &other}
	if _, _, err := login(newAuth(t, db), "alice", "correct horse battery"); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("Login after a reset: %v, want auth.ErrAuthFailed", err)
	}
	if len(db.minted) != 0 {
		t.Fatal("a session was minted for a password that is no longer current")
	}
}

// Anti-oracle: an unknown AID, an operator of another method and a wrong
// password give the caller one and the same answer.
func TestLogin_EveryCredentialRefusalIsTheSame(t *testing.T) {
	h := fixtureHash
	db := &fakeDB{rows: map[string]row{
		"alice":       {method: "local", hash: &h},
		"archon-root": {method: "jwt", hash: &h},
		"ldap-user":   {method: "ldap"},
	}}
	a := newAuth(t, db)
	for name, c := range map[string][2]string{
		"wrong password":       {"alice", "not the password"},
		"unknown AID":          {"nobody", "correct horse battery"},
		"jwt operator":         {"archon-root", "correct horse battery"},
		"ldap operator":        {"ldap-user", "correct horse battery"},
		"AID outside the form": {"../etc", "correct horse battery"},
		"empty password":       {"alice", ""},
	} {
		if _, _, err := login(a, c[0], c[1]); !errors.Is(err, auth.ErrAuthFailed) {
			t.Errorf("%s: %v, want auth.ErrAuthFailed", name, err)
		}
	}
	if len(db.minted) != 0 {
		t.Fatalf("a refused login minted %v", db.minted)
	}
}

func TestLogin_RevokedOnlyAfterTheRightPassword(t *testing.T) {
	h := fixtureHash
	db := &fakeDB{rows: map[string]row{"alice": {method: "local", revoked: true, hash: &h}}}
	a := newAuth(t, db)
	if _, _, err := login(a, "alice", "correct horse battery"); !errors.Is(err, auth.ErrOperatorRevoked) {
		t.Fatalf("revoked, right password: %v, want auth.ErrOperatorRevoked", err)
	}
	if _, _, err := login(a, "alice", "not the password"); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("revoked, wrong password: %v, want auth.ErrAuthFailed", err)
	}
}

func TestLogin_DoesNotHashAnOversizedPassword(t *testing.T) {
	db := &fakeDB{}
	big := make([]byte, maxPasswordBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if _, _, err := login(newAuth(t, db), "alice", string(big)); !errors.Is(err, auth.ErrAuthFailed) {
		t.Fatalf("oversized password: %v, want auth.ErrAuthFailed", err)
	}
	if len(db.queried) != 0 {
		t.Fatal("an oversized password reached the lookup")
	}
}
