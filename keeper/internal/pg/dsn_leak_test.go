package pg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

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
