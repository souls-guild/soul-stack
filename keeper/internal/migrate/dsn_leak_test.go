package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/migrations"
)

// isolatePGEnv takes the libpq environment variables out of the parser's view.
//
// pgx merges `PG*` into every parse it performs, so a `PGSERVICE` or `PGSSLMODE`
// left over in a shell decides the outcome of a table that names neither — red
// about a DSN nobody touched, pointing at the DSN rather than at the shell.
//
// Clearing rather than unsetting is deliberate: pgconn's parseEnvSettings skips
// empty values, so an empty variable is a real "not set" for the parser — and
// [testing.T.Setenv] restores the developer's shell after.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "PG") {
			t.Setenv(name, "")
		}
	}
}

// leakPassword is the password planted in every DSN below. It is distinctive on
// purpose: a substring search for it is only meaningful if the token cannot
// occur in an error by coincidence.
const leakPassword = "s3cr3t-pg-passw0rd"

// assertNoLeak fails when err's message carries the password, or any longer run
// of the DSN that would contain it.
//
// It reads the message the operator would actually see — the full wrapped chain
// — rather than the format string of any one layer. A leak reintroduced three
// wraps down is the same leak (NIM-418).
func assertNoLeak(t *testing.T, dsn string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, leakPassword) {
		t.Errorf("error message leaks the password: %s", msg)
	}
	// The userinfo is the credential-bearing segment; if any of it survived, the
	// password did too, whatever the password happens to be — which is the point
	// of the arm, since the check above only knows the one constant planted here.
	//
	// The cut is on `://` rather than off a `postgres://` prefix, because most
	// of what this package refuses is not a postgres URL: `mysql://`, `pgx5://`,
	// and keyword/value with no scheme at all. Trimming a prefix that is not
	// there left the whole DSN to be cut on `@`, so the arm looked for the
	// scheme as well and went dead on exactly those rows. Measured before
	// changing it: a planted leak of the authority was still caught, by the
	// constant above. So this is the arm doing its own job again, not a hole.
	//
	// Neither arm sees `user=keeper database=keeper`, which pgconn does emit.
	// That is not a credential, and it is noted so nobody reads this helper as
	// asserting that nothing of the DSN survives.
	if scheme, rest, ok := strings.Cut(dsn, "://"); ok && scheme != "" && !strings.ContainsAny(scheme, "= ") {
		if user, _, ok := strings.Cut(rest, "@"); ok && user != "" {
			if strings.Contains(msg, user) {
				t.Errorf("error message leaks the DSN userinfo %q: %s", user, msg)
			}
		}
	}
}

// TestToMigrateURL_ErrorsCarryNoDSN walks every rejecting branch of
// [toMigrateURL] with a password-bearing DSN.
//
// Each case pins the sentinel it must reach. Without that, a table can drift
// until every row lands in the same branch and the test still passes while
// claiming coverage it no longer has.
func TestToMigrateURL_ErrorsCarryNoDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want error
		// class, when set, is the fault [describeParseFailure] must name. It
		// pins which arm of the classifier ran; without it a table row proves
		// only that something failed.
		class string
	}{
		{
			// The form that reaches production: libpq keyword/value is a DSN
			// pg.NewPool accepts, so the pool connects and this is the first
			// step to refuse it.
			name: "keyword/value has no scheme",
			dsn:  "host=localhost user=keeper password=" + leakPassword + " dbname=keeper",
			want: ErrUnsupportedDSNScheme,
		},
		{
			name: "wrong scheme",
			dsn:  "mysql://keeper:" + leakPassword + "@localhost:3306/keeper",
			want: ErrUnsupportedDSNScheme,
		},
		{
			// A naive split on "://" would report the password as the scheme.
			name: "keyword/value whose password contains a scheme separator",
			dsn:  "host=localhost password=" + leakPassword + "://x dbname=keeper",
			want: ErrUnsupportedDSNScheme,
		},
		{
			name: "empty",
			dsn:  "",
			want: ErrUnsupportedDSNScheme,
		},
		{
			// net/url rejects the bare `%`; its *url.Error keeps the whole URL
			// and quotes the offending run of the password.
			name:  "postgres URL with an invalid percent-escape",
			dsn:   "postgres://keeper:" + leakPassword + "%@localhost:5432/keeper",
			want:  ErrMalformedDSN,
			class: "contains an invalid percent-escape",
		},
		{
			name:  "pgx5 URL with an invalid percent-escape",
			dsn:   "pgx5://keeper:" + leakPassword + "%@localhost:5432/keeper",
			want:  ErrMalformedDSN,
			class: "contains an invalid percent-escape",
		},
		{
			// The worst shape, and the reason the cause is dropped rather than
			// wrapped: `#` opens a fragment, so the parse fails past it and
			// net/url names the password twice —
			// `parse "pgx5://keeper:<pw>": invalid port ":<pw>" after host`.
			// Both halves are plain errors, so no type can single them out.
			name:  "password containing a fragment marker",
			dsn:   "postgres://keeper:" + leakPassword + "#x@localhost:5432/keeper",
			want:  ErrMalformedDSN,
			class: "cause withheld",
		},
		{
			name:  "password containing a raw space",
			dsn:   "postgres://keeper:" + leakPassword + " x@localhost:5432/keeper",
			want:  ErrMalformedDSN,
			class: "cause withheld",
		},
		{
			name:  "invalid character in the host",
			dsn:   "postgres://keeper:" + leakPassword + "@ho|st:5432/keeper",
			want:  ErrMalformedDSN,
			class: "contains an invalid character in the host name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toMigrateURL(tc.dsn)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v (branch not reached — the leak check below would prove nothing)", err, tc.want)
			}
			if got != "" {
				t.Errorf("returned URL %q alongside an error", got)
			}
			if tc.class != "" && !strings.Contains(err.Error(), tc.class) {
				t.Errorf("err = %v, want the fault classified as %q", err, tc.class)
			}
			assertNoLeak(t, tc.dsn, err)
		})
	}
}

// TestToMigrateURL_AcceptsPasswordWithReservedCharacters keeps the fix from
// turning into a denial of service: a percent-ENCODED password is legal in a
// URL, and rejecting it would lock out operators whose password contains `@`,
// `/` or `%`.
func TestToMigrateURL_AcceptsPasswordWithReservedCharacters(t *testing.T) {
	const encoded = "p%40ss%2Fw%25rd"
	got, err := toMigrateURL("postgres://keeper:" + encoded + "@localhost:5432/keeper?sslmode=disable")
	if err != nil {
		t.Fatalf("toMigrateURL: %v", err)
	}
	if want := "pgx5://keeper:" + encoded + "@localhost:5432/keeper?sslmode=disable"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestApply_ErrorsCarryNoDSN is the end-to-end half: the message an operator
// reads is the one [Apply] returns, after golang-migrate and pgx have had their
// turn at it.
//
// The last case is the point of the test. It gets past our own validation and
// into third-party code with a real password in hand, which is the only way to
// find out whether that code echoes it — reading their source proves what one
// version does today.
func TestApply_ErrorsCarryNoDSN(t *testing.T) {
	isolatePGEnv(t)

	cases := []struct {
		name string
		dsn  string
		// want is the sentinel the row has to reach. A nil want means the row
		// must NOT stop at one — see the last case, which earns its keep only by
		// getting past our own validation.
		want error
	}{
		{"keyword/value", "host=localhost user=keeper password=" + leakPassword + " dbname=keeper", ErrUnsupportedDSNScheme},
		{"wrong scheme", "mysql://keeper:" + leakPassword + "@localhost:3306/keeper", ErrUnsupportedDSNScheme},
		{"invalid percent-escape", "postgres://keeper:" + leakPassword + "%@localhost:5432/keeper", ErrMalformedDSN},
		{"fragment marker", "postgres://keeper:" + leakPassword + "#x@localhost:5432/keeper", ErrMalformedDSN},
		{
			// Well-formed and unreachable: golang-migrate opens the database
			// driver, pgx dials, the connection is refused.
			//
			// `connect_timeout` is not decoration. golang-migrate pings through
			// `database/sql` with no context — `Apply`'s ctx does not reach it —
			// and pgx sets a dial deadline only when the DSN asks for one. On a
			// host that drops rather than refuses, the omission is this test
			// hanging for the kernel's SYN-retry budget inside `make check`.
			//
			// `sslmode=disable` is load-bearing for the same reason and less
			// obviously: with it pgx builds no TLS fallback, without it one, and
			// each fallback is dialled in turn — so dropping it doubles the
			// bound that `connect_timeout` was added to impose.
			name: "well-formed URL, connection refused",
			dsn:  "postgres://keeper:" + leakPassword + "@127.0.0.1:1/keeper?sslmode=disable&connect_timeout=2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Apply(context.Background(), tc.dsn, migrations.FS, ".")
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			// Without this, a refusal moved earlier — a stricter check on
			// `dsn_ref`, say — leaves every row green having never run the step
			// it was written for. The last row fails the other way round: if it
			// ever stops at our own validation it is no longer reaching the
			// third-party code it exists to interrogate.
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v (branch not reached — the leak check below would prove nothing)", err, tc.want)
			case tc.want == nil && (errors.Is(err, ErrUnsupportedDSNScheme) || errors.Is(err, ErrMalformedDSN)):
				t.Fatalf("err = %v, want a failure past our own validation", err)
			}
			assertNoLeak(t, tc.dsn, err)
		})
	}
}
