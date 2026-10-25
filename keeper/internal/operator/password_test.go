package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fastHashes swaps the argon2 cost for a cheap one for the test: the
// production cost is ~100 ms a hash and several times that under -race.
func fastHashes(t *testing.T) {
	t.Helper()
	prev := newHashParams
	newHashParams = argonParams{time: 1, memory: 1024, threads: 1, keyLen: 32, saltLen: 16}
	t.Cleanup(func() { newHashParams = prev })
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := hashPassword(context.Background(), pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

// loginRowDB answers selectPasswordLoginSQL with one row (or none).
type loginRowDB struct {
	found   bool
	method  AuthMethod
	revoked bool
	hash    *string
}

func (d *loginRowDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("loginRowDB: unexpected Exec")
}

func (d *loginRowDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("loginRowDB: unexpected Query")
}

func (d *loginRowDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if sql != selectPasswordLoginSQL {
		return errRow{err: errors.New("loginRowDB: unexpected query")}
	}
	if !d.found {
		return errRow{err: pgx.ErrNoRows}
	}
	return loginRow{d}
}

type loginRow struct{ d *loginRowDB }

func (r loginRow) Scan(dest ...any) error {
	*dest[0].(*string) = string(r.d.method)
	*dest[1].(*bool) = r.d.revoked
	*dest[2].(**string) = r.d.hash
	return nil
}

func TestHashParams_AreRFC9106SecondRecommendation(t *testing.T) {
	want := argonParams{time: 3, memory: 64 * 1024, threads: 4, keyLen: 32, saltLen: 16}
	if newHashParams != want {
		t.Fatalf("argon2id parameters = %+v, want %+v (RFC 9106 §4, second recommended option)", newHashParams, want)
	}
}

func TestHashPassword_RoundTripAndFreshSalt(t *testing.T) {
	fastHashes(t)
	ctx := context.Background()
	a, b := mustHash(t, "correct horse battery"), mustHash(t, "correct horse battery")
	if a == b {
		t.Fatal("two hashes of one password are equal: the salt is not fresh")
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("hash %q is not PHC argon2id with the parameters it was made with", a)
	}
	for _, c := range []struct {
		pw   string
		want bool
	}{{"correct horse battery", true}, {"correct horse batterY", false}, {"", false}} {
		ok, err := verifyPassword(ctx, a, c.pw)
		if err != nil || ok != c.want {
			t.Errorf("verify(%q) = %v, %v; want %v, nil", c.pw, ok, err, c.want)
		}
	}
}

// The parameters are read back from the stored string, so a stored hash keeps
// verifying after the defaults change.
func TestVerifyPassword_UsesTheStoredParameters(t *testing.T) {
	fastHashes(t)
	h := mustHash(t, "a long enough password")
	newHashParams = argonParams{time: 2, memory: 2048, threads: 2, keyLen: 32, saltLen: 16}
	if ok, err := verifyPassword(context.Background(), h, "a long enough password"); err != nil || !ok {
		t.Fatalf("verify after a parameter change = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyPassword_RefusesAMalformedOrHostileHash(t *testing.T) {
	fastHashes(t)
	good := mustHash(t, "a long enough password")
	parts := strings.Split(good, "$")
	for name, h := range map[string]string{
		"not argon2id":     strings.Replace(good, "argon2id", "argon2i", 1),
		"wrong version":    strings.Replace(good, "v=19", "v=16", 1),
		"memory too large": strings.Join(append(append([]string{}, parts[:3]...), "m=4194304,t=1,p=1", parts[4], parts[5]), "$"),
		"zero time":        strings.Join(append(append([]string{}, parts[:3]...), "m=1024,t=0,p=1", parts[4], parts[5]), "$"),
		"truncated":        strings.Join(parts[:5], "$"),
		"bad salt":         strings.Join(append(append([]string{}, parts[:4]...), "!!", parts[5]), "$"),
	} {
		if ok, err := verifyPassword(context.Background(), h, "a long enough password"); err == nil || ok {
			t.Errorf("%s: verify = %v, %v; want an error", name, ok, err)
		}
	}
}

func TestPasswordPolicy_Check(t *testing.T) {
	p := PasswordPolicy{MinLength: 12}
	for _, c := range []struct {
		name, pw string
		ok       bool
	}{
		{"exactly the minimum", "abcdefghijkl", true},
		{"one short", "abcdefghijk", false},
		{"counts characters, not bytes", "пароль-пароль", true},
		{"spaces are fine", "correct horse battery", true},
		{"at the maximum", strings.Repeat("x", MaxPasswordLength), true},
		{"over the maximum", strings.Repeat("x", MaxPasswordLength+1), false},
		{"a newline", "abcdefghijkl\n", false},
		{"invalid UTF-8", "abcdefghijk\xff", false},
		{"equals the AID, any case", "ALICE-OPERATOR", false},
	} {
		err := p.Check("alice-operator", c.pw)
		if (err == nil) != c.ok {
			t.Errorf("%s: Check = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrWeakPassword) {
			t.Errorf("%s: %v is not ErrWeakPassword", c.name, err)
		}
	}
}

// The anti-takeover invariant (ADR-058(d), NIM-1051): a password logs in only an
// operator of method `local`. Even a row that somehow carries a valid hash for a
// `jwt` operator — the bootstrap Archon, a service account — is refused.
func TestVerifyLocalPassword_RefusesAnOperatorOfAnotherMethod(t *testing.T) {
	fastHashes(t)
	h := mustHash(t, "the right password")
	for _, m := range []AuthMethod{AuthMethodJWT, AuthMethodLDAP, AuthMethodOIDC, AuthMethodMTLS, AuthMethodCombined} {
		db := &loginRowDB{found: true, method: m, hash: &h}
		if _, err := VerifyLocalPassword(context.Background(), db, "archon-root", "the right password"); !errors.Is(err, ErrNotLocalOperator) {
			t.Errorf("method %s with the right password: %v, want ErrNotLocalOperator", m, err)
		}
	}
}

func TestVerifyLocalPassword_Verdicts(t *testing.T) {
	fastHashes(t)
	h := mustHash(t, "the right password")
	ctx := context.Background()
	for _, c := range []struct {
		name string
		db   *loginRowDB
		pw   string
		want error
	}{
		{"match", &loginRowDB{found: true, method: AuthMethodLocal, hash: &h}, "the right password", nil},
		{"mismatch", &loginRowDB{found: true, method: AuthMethodLocal, hash: &h}, "the wrong password", ErrPasswordMismatch},
		{"unknown AID", &loginRowDB{}, "the right password", ErrOperatorNotFound},
		{"local without a hash", &loginRowDB{found: true, method: AuthMethodLocal}, "the right password", ErrNotLocalOperator},
		{"revoked, right password", &loginRowDB{found: true, method: AuthMethodLocal, revoked: true, hash: &h}, "the right password", ErrOperatorAlreadyRevoked},
		// Revocation is told only to whoever knows the password.
		{"revoked, wrong password", &loginRowDB{found: true, method: AuthMethodLocal, revoked: true, hash: &h}, "the wrong password", ErrPasswordMismatch},
	} {
		check, err := VerifyLocalPassword(ctx, c.db, "alice", c.pw)
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if (err == nil) != (check.AID() == "alice") {
			t.Errorf("%s: check for %q with err %v; a check exists exactly when the password verified", c.name, check.AID(), err)
		}
	}
}

func TestNormalizeLoginAID(t *testing.T) {
	if got := NormalizeLoginAID("  Alice.Ops \t"); got != "alice.ops" {
		t.Fatalf("NormalizeLoginAID = %q, want alice.ops", got)
	}
}

func TestWithHashSlot_HonoursTheContext(t *testing.T) {
	for i := 0; i < cap(hashSlots); i++ {
		hashSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(hashSlots); i++ {
			<-hashSlots
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashPassword(ctx, "a long enough password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("HashPassword with every slot taken and a cancelled context: %v, want context.Canceled", err)
	}
}
