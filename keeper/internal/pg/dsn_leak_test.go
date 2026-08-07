package pg

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/config"
)

// leakPassword is the password planted in every DSN below. It is distinctive on
// purpose: a substring search for it is only meaningful if the token cannot
// occur in an error by coincidence.
const leakPassword = "s3cr3t-pg-passw0rd"

// isolatePGEnv takes the libpq environment variables out of the parser's view.
//
// pgx merges `PG*` into EVERY parse, the successful ones included, so a
// `PGSERVICE` or `PGSSLMODE` left over in a shell decides the outcome of tables
// below that name neither. The failure that produces is the worst kind: red
// about a DSN nobody touched, pointing at the DSN rather than at the shell.
//
// Clearing rather than unsetting is deliberate and load-bearing: pgconn's
// parseEnvSettings skips empty values, so an empty variable is a real "not set"
// for the parser — and [testing.T.Setenv] restores the developer's shell after.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "PG") {
			t.Setenv(name, "")
		}
	}
}

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
	// In URL form the userinfo is the credential-bearing segment; if any of it
	// survived, the password did too, whatever the password happens to be.
	if isURLDSN(dsn) {
		_, rest, _ := strings.Cut(dsn, "://")
		if user, _, ok := strings.Cut(rest, "@"); ok && user != "" {
			if strings.Contains(msg, user) {
				t.Errorf("error message leaks the DSN userinfo %q: %s", user, msg)
			}
		}
	}
}

// TestParsePoolConfig_ErrorsCarryNoDSN walks the refusing branches of
// [parsePoolConfig] with a password-bearing DSN.
//
// The keyword/value rows are not padding. pgx redacts the connection string it
// quotes with a regex over `password=`, and libpq accepts whitespace around the
// `=` — so `password = x` and `password= x` slip past that regex and land in
// the log verbatim, including on failures that have nothing to do with the
// password (a mistyped `sslmode`). That is why no pgx error is wrapped here,
// only re-described.
func TestParsePoolConfig_ErrorsCarryNoDSN(t *testing.T) {
	isolatePGEnv(t)

	urlWith := func(passwordTail, query string) string {
		return "postgres://keeper:" + leakPassword + passwordTail + "@10.0.0.5:5432/keeper?" + query
	}
	kvWith := func(password, tail string) string {
		return "host=10.0.0.5 user=keeper " + password + " " + tail
	}

	cases := []struct {
		name string
		dsn  string
		// class, when set, is the text the message must carry. It pins which
		// arm ran; without it a table row proves only that something failed.
		class string
	}{
		{
			// The shape that put this ticket in the tracker: `#` opens a
			// fragment, so the parse fails past it and net/url reports
			// `invalid port ":<password>" after host`. pgx returns that error
			// with its own wrapper stripped, so nothing upstream redacts it.
			name:  "password containing a fragment marker",
			dsn:   urlWith("#x", "sslmode=verify-full"),
			class: withheldCause,
		},
		{
			name:  "password containing a path separator",
			dsn:   urlWith("/x", "sslmode=verify-full"),
			class: withheldCause,
		},
		{
			name:  "password containing a query marker",
			dsn:   urlWith("?x", "sslmode=verify-full"),
			class: withheldCause,
		},
		{
			name:  "password containing a raw space",
			dsn:   urlWith(" x", "sslmode=verify-full"),
			class: withheldCause,
		},
		{
			name:  "password with an invalid percent-escape",
			dsn:   urlWith("%zz", "sslmode=verify-full"),
			class: "contains an invalid percent-escape",
		},
		{
			name:  "invalid character in the host",
			dsn:   "postgres://keeper:" + leakPassword + "@ho|st:5432/keeper",
			class: "contains an invalid character in the host name",
		},
		{
			// Well-formed URL, unrelated fault. pgx's diagnosis is worth
			// keeping — it is the difference between "fix the DSN" and "fix
			// this one setting" — so the message must name the setting and
			// still not name the password.
			name:  "well-formed URL, invalid sslmode",
			dsn:   urlWith("", "sslmode=bogus"),
			class: "sslmode is invalid",
		},
		{
			name:  "well-formed URL, invalid pool setting",
			dsn:   urlWith("", "pool_max_conns=abc"),
			class: "cannot parse pool_max_conns",
		},
		{
			// The password is in the query rather than the userinfo, which is
			// the half pgx's URL redaction does not cover.
			name:  "password as a query parameter",
			dsn:   "postgres://keeper@10.0.0.5:5432/keeper?password=" + leakPassword + "&sslmode=bogus",
			class: "sslmode is invalid",
		},
		{
			name:  "keyword/value, invalid syntax",
			dsn:   kvWith("password="+leakPassword, "dbname"),
			class: "invalid keyword/value",
		},
		{
			name:  "keyword/value with spaces around =, invalid syntax",
			dsn:   kvWith("password = "+leakPassword, "dbname"),
			class: "invalid keyword/value",
		},
		{
			name:  "keyword/value with a space after =, invalid sslmode",
			dsn:   kvWith("password= "+leakPassword, "sslmode=bogus"),
			class: "sslmode is invalid",
		},
		{
			name:  "keyword/value quoted with spaces around =, invalid sslmode",
			dsn:   kvWith("password = '"+leakPassword+"'", "sslmode=bogus"),
			class: "sslmode is invalid",
		},
		{
			name:  "keyword/value, unterminated quote",
			dsn:   "host=10.0.0.5 user=keeper password='" + leakPassword,
			class: "unterminated quoted string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parsePoolConfig(tc.dsn)
			if !errors.Is(err, ErrMalformedDSN) {
				t.Fatalf("err = %v, want ErrMalformedDSN (branch not reached — the leak check below would prove nothing)", err)
			}
			if cfg != nil {
				t.Error("returned a config alongside an error")
			}
			if !strings.Contains(err.Error(), tc.class) {
				t.Errorf("err = %v, want the fault described as %q", err, tc.class)
			}
			assertNoLeak(t, tc.dsn, err)
		})
	}
}

// TestParsePoolConfig_AcceptsLegitimateDSNs keeps the fix from turning into a
// denial of service. Refusing a DSN the keeper used to run on is the same
// outage as leaking, arrived at from the other side, and the net/url gate is
// the part that could cause it.
func TestParsePoolConfig_AcceptsLegitimateDSNs(t *testing.T) {
	isolatePGEnv(t)

	cases := []struct {
		name string
		dsn  string
	}{
		{"plain URL", "postgres://keeper:" + leakPassword + "@10.0.0.5:5432/keeper?sslmode=verify-full"},
		{"postgresql scheme", "postgresql://keeper:" + leakPassword + "@10.0.0.5:5432/keeper"},
		{"percent-encoded reserved characters", "postgres://keeper:p%40ss%2Fw%25rd@10.0.0.5:5432/keeper"},
		{"password containing @ and :", "postgres://keeper:" + leakPassword + "@x:y@10.0.0.5:5432/keeper"},
		{"empty password", "postgres://keeper:@10.0.0.5:5432/keeper"},
		{"no userinfo", "postgres://10.0.0.5:5432/keeper"},
		{"multi-host", "postgres://keeper:" + leakPassword + "@h1:5432,h2:5432/keeper?target_session_attrs=primary"},
		{"IPv6 literal", "postgres://keeper:" + leakPassword + "@[2001:db8::1]:5432/keeper"},
		{"unix socket via host parameter", "postgres://keeper@/keeper?host=/var/run/postgresql"},
		{"pool settings", "postgres://keeper:" + leakPassword + "@10.0.0.5:5432/keeper?pool_max_conns=10"},
		{"keyword/value", "host=10.0.0.5 port=5432 user=keeper password=" + leakPassword + " dbname=keeper"},
		{"keyword/value with spaces around =", "host=10.0.0.5 user=keeper password = " + leakPassword + " dbname=keeper"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parsePoolConfig(tc.dsn)
			if err != nil {
				t.Fatalf("parsePoolConfig refused a DSN pgx accepts: %v", err)
			}
			if cfg == nil {
				t.Fatal("nil config without an error")
			}
		})
	}
}

// TestNewPool_ErrorsCarryNoDSN is the end-to-end half: the string an operator
// reads is the one [NewPool] returns, after every wrap between here and stderr.
//
// `dsn_ref` may hold the DSN inline rather than a `vault:` ref, which is the
// path a leak takes with no Vault in the picture at all.
func TestNewPool_ErrorsCarryNoDSN(t *testing.T) {
	isolatePGEnv(t)

	cases := []struct {
		name string
		dsn  string
	}{
		{"fragment marker", "postgres://keeper:" + leakPassword + "#x@10.0.0.5:5432/keeper"},
		{"path separator", "postgres://keeper:" + leakPassword + "/x@10.0.0.5:5432/keeper"},
		{"invalid sslmode", "postgres://keeper:" + leakPassword + "@10.0.0.5:5432/keeper?sslmode=bogus"},
		{"keyword/value with spaces around =", "host=10.0.0.5 user=keeper password = " + leakPassword + " dbname"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPool(context.Background(), config.KeeperPostgres{DSNRef: tc.dsn}, nil)
			// Same reason as the table above, and more pressing here: NewPool
			// resolves the ref before it parses, so a later refusal on the
			// resolve step would leave these rows passing while the parse
			// branch they exist for never runs.
			if !errors.Is(err, ErrMalformedDSN) {
				t.Fatalf("err = %v, want ErrMalformedDSN (parse branch not reached)", err)
			}
			assertNoLeak(t, tc.dsn, err)
		})
	}
}

// TestDescribeParseConfigFailure_Containment drives the containment check in
// [describeParseConfigFailure] directly, because no DSN can.
//
// Every cause today's pgx reports names a setting, a host or a file path, so
// not one row of the tables above reaches that check — replacing its body with
// a panic leaves the package green. Which is the point of it: it is the belt
// for the pgx that has not shipped yet, and a guard against a future version is
// worth exactly nothing if the only thing that could trip it is that future
// version. So the cause is supplied here instead of waited for.
//
// The rows go both ways on purpose. A check that withholds everything would
// pass a table of leaks alone, and would cost the operator the diagnosis in
// every startup failure — refusing to start over a value only Vault can show
// you is a bad enough place to be without the reason being withheld too.
func TestDescribeParseConfigFailure_Containment(t *testing.T) {
	// Distinct from leakPassword: this password never travels through a parser
	// here, it is planted straight into a cause.
	const secret = "s3cr3t-in-the-cause"
	connString := "postgres://keeper:" + secret + "@10.0.0.5:5432/keeper"

	cases := []struct {
		name    string
		err     error
		secrets []string
		want    string
	}{
		{
			// Today's shape. The secret sits in the connection string and the
			// cause is clean, so blanking ConnString is the whole job and the
			// diagnosis survives — the difference between "fix the DSN" and
			// "fix this one setting".
			name:    "cause names a setting, connection string names the secret",
			err:     pgconn.NewParseConfigError(connString, "failed to configure TLS", errors.New("sslmode is invalid")),
			secrets: []string{secret},
			want:    "failed to configure TLS (sslmode is invalid)",
		},
		{
			// Tomorrow's shape: the wording net/url already produces one layer
			// up, moved into the cause. Blanking ConnString does nothing for it.
			name:    "cause names the secret",
			err:     pgconn.NewParseConfigError(connString, "failed to parse as URL", errors.New(`invalid port ":`+secret+`" after host`)),
			secrets: []string{secret},
			want:    withheldCause,
		},
		{
			// Not every error pgx returns is a ParseConfigError, and the ones
			// that are not carry no ConnString to blank — the containment check
			// is all that stands between their text and the log.
			name:    "plain error naming the secret",
			err:     errors.New(`invalid port ":` + secret + `" after host`),
			secrets: []string{secret},
			want:    withheldCause,
		},
		{
			// `postgres://keeper:@10.0.0.5/keeper` is a legal DSN, and its
			// password is the empty string. Containment on a token that occurs
			// in every message would withhold every diagnosis the keeper has.
			name:    "empty password is not a token to contain",
			err:     pgconn.NewParseConfigError("postgres://keeper:@10.0.0.5:5432/keeper", "failed to configure TLS", errors.New("sslmode is invalid")),
			secrets: []string{""},
			want:    "failed to configure TLS (sslmode is invalid)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeParseConfigFailure(tc.err, tc.secrets); got != tc.want {
				t.Errorf("describeParseConfigFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRawURLSecrets pins the as-written half of what the containment check is
// given.
//
// [url.URL] decodes: a DSN spelled `p%40ss` reaches `u.User.Password()` as
// `p@ss`, and a message that quoted the connection string verbatim would carry
// the first form while containment looked for the second. Verbatim quoting is
// the disclosure this check exists to survive, so the encoded run has to be in
// the list — and nothing in the tables above can prove it is, because no pgx
// error today quotes a connection string it has not already redacted.
func TestRawURLSecrets(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want []string
	}{
		{
			// The row that motivates the function: encoded and decoded differ.
			name: "percent-encoded password",
			dsn:  "postgres://keeper:p%40ss%2Fw%25rd@10.0.0.5:5432/keeper",
			want: []string{"p%40ss%2Fw%25rd"},
		},
		{
			// net/url ends the userinfo at its LAST `@` and starts the password
			// after the FIRST `:`; both characters are legal raw in a password,
			// and reading either boundary the other way truncates the secret.
			name: "password containing @ and :",
			dsn:  "postgres://keeper:pw@x:y@10.0.0.5:5432/keeper",
			want: []string{"pw@x:y"},
		},
		{
			// The half pgx's own URL redaction does not cover at all.
			name: "password as a query parameter",
			dsn:  "postgres://keeper@10.0.0.5:5432/keeper?password=p%40ss&sslmode=require",
			want: []string{"p%40ss"},
		},
		{
			name: "no userinfo",
			dsn:  "postgres://10.0.0.5:5432/keeper",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.dsn)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			got := rawURLSecrets(tc.dsn, u)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rawURLSecrets = %q, want %q", got, tc.want)
			}
			// The claim is not "some string came back" but "a string the
			// decoded form would not have matched".
			if decoded, ok := u.User.Password(); ok && len(got) > 0 && decoded == got[0] && strings.Contains(tc.dsn, "%") {
				t.Errorf("raw and decoded password are both %q — the encoded spelling is not being kept", decoded)
			}
		})
	}
}
