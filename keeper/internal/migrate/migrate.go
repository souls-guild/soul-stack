// Package migrate applies embedded SQL migrations from `keeper/migrations/`
// against the Keeper's Postgres. Integrated into `keeper/cmd/keeper/main.go`
// (apply-on-startup) since M0.4.2; in M0.4.0 this package was invoked
// ad-hoc (smoke test).
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers driver "pgx5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// A DSN is a credential. Every error this package returns travels to the
// keeper's stderr — `keeper run: apply migrations: …` — which in a cluster
// deployment is a pod log shipped to central collection. So no error here may
// carry the DSN or any slice of it: not the value, not the userinfo, not the
// offending fragment a parser quotes back. What an operator needs to fix a bad
// `dsn_ref` is the FORM that was expected and the form that arrived, and the
// scheme carries all of that (NIM-418).
//
// Two sentinels, because the two failures have different fixes: a wrong scheme
// means the DSN is in the wrong dialect, a malformed URL means it is the right
// dialect written wrong.
var (
	// ErrUnsupportedDSNScheme reports a DSN that is not a Postgres URL — most
	// often the libpq keyword/value form (`host=… password=…`), which
	// `keeper/internal/pg.NewPool` accepts and golang-migrate does not. That
	// asymmetry is how this reaches production: the pool connects, the
	// migration step refuses, and the refusal is the first thing to see the DSN.
	ErrUnsupportedDSNScheme = errors.New("migrate: dsn must be postgres:// or postgresql:// URL")

	// ErrMalformedDSN reports a Postgres URL that net/url rejects. It carries a
	// class of fault from describeParseFailure, never the text that caused it.
	//
	// The cause is deliberately dropped rather than wrapped, because *url.Error
	// discloses on both halves at once: it keeps the whole URL verbatim, and the
	// error inside it quotes what the parser choked on. A password with a bare
	// `%` yields `invalid URL escape "%"`; a password with a `#` is worse still,
	// since `#` opens a fragment and the parse fails past it — the message
	// becomes `parse "pgx5://keeper:s3cr3t": invalid port ":s3cr3t" after host`,
	// naming the password twice. A partial disclosure is still a disclosure.
	ErrMalformedDSN = errors.New("migrate: dsn is not a valid URL")
)

// Apply runs all up migrations from `fs` (sub-path `subdir`, e.g. `"."`) over
// Postgres by `dsn`. It is idempotent: if all migrations are already applied,
// it returns nil (golang-migrate returns `migrate.ErrNoChange` in that case,
// and we swallow it).
//
// `migrate` uses its own sql.DB (through registered driver `pgx5`), separate
// from `keeper/internal/pg.NewPool`. This is by design: migrate tool requires
// an exclusive lock through `pg_advisory_lock` while running; mixing pool and
// migrate connection is dangerous (deadlock potential). Apply opens its own
// conn and closes it after Up.
func Apply(ctx context.Context, dsn string, fs embed.FS, subdir string) error {
	src, err := iofs.New(fs, subdir)
	if err != nil {
		return fmt.Errorf("migrate: iofs source: %w", err)
	}

	// golang-migrate expects URL like `pgx5://...`. DSN may arrive as
	// `postgres://...` (standard), so replace scheme; pgx ParseConfig already
	// accepted such DSN in NewPool.
	migrateURL, err := toMigrateURL(dsn)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("migrate: new instance: %w", err)
	}
	defer m.Close() //nolint:errcheck // close errors are not critical after successful Up

	// ctx-cancellation: golang-migrate/v4 API does not accept context; the only
	// way to interrupt a long Up is a signal to m.GracefulStop (chan bool,
	// signal-only). Goroutine waits for ctx.Done and sends signal; local done
	// channel silences it after successful Up so it does not hang on parent ctx
	// after return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// non-blocking: GracefulStop is buffered chan bool(1), repeat signal
			// is not needed.
			select {
			case m.GracefulStop <- true:
			default:
			}
		case <-done:
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}

// toMigrateURL replaces scheme `postgres://` / `postgresql://` with
// `pgx5://` (name of registered database/pgx/v5 driver). Other schemes
// (including keyvalue format) are rejected; keeper.yml canonicalizes URL form.
//
// Errors are [ErrUnsupportedDSNScheme] / [ErrMalformedDSN] and never carry the
// DSN — see the note on those sentinels.
func toMigrateURL(dsn string) (string, error) {
	var migrateURL string
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		migrateURL = "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		migrateURL = "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	case strings.HasPrefix(dsn, "pgx5://"):
		migrateURL = dsn
	default:
		return "", fmt.Errorf("%w (%s)", ErrUnsupportedDSNScheme, describeScheme(dsn))
	}

	// golang-migrate re-parses this string with net/url and returns the
	// resulting *url.Error unchanged — an error that keeps the URL verbatim,
	// password included, all the way up to our `migrate: new instance: %w`.
	// Parsing here first makes that echo unreachable rather than merely
	// unreached: whatever net/url accepts now, it accepts again downstream.
	//
	// This cannot refuse a DSN the keeper otherwise runs on. Both call sites
	// open the pool before migrating, and pgconn.ParseConfig dispatches on the
	// same two prefixes and runs net/url on them itself — so a URL that reaches
	// here has already been through this exact parser. Multi-host, IPv6 with a
	// zone, `?host=/var/run/postgresql`, an empty or percent-encoded password
	// all parse; a raw `#` or space in the password does not, and does not get
	// past the pool either.
	if _, err := url.Parse(migrateURL); err != nil {
		return "", fmt.Errorf("%w (%s)", ErrMalformedDSN, describeParseFailure(err))
	}
	return migrateURL, nil
}

// describeParseFailure names the class of a net/url failure without quoting the
// text that failed.
//
// Type and value disclose differently, and that gap is the whole function:
// [url.EscapeError]'s TYPE says "there is a bare % somewhere in here", while its
// VALUE is three characters of the password. So the classes below are read off
// the error's type and every branch returns a constant — no input reaches the
// output, which is what keeps a diagnostic from becoming a leak.
//
// The two named classes cover the causes an operator actually hits: a `%`, `#`
// or space in an unencoded password, and a stray character in the host. The
// rest stay unnamed on purpose — net/url reports `invalid port ":s3cr3t" after
// host` and `net/url: invalid userinfo` as plain errors, indistinguishable by
// type, and the first of those quotes the password.
func describeParseFailure(err error) string {
	var escape url.EscapeError
	if errors.As(err, &escape) {
		return "contains an invalid percent-escape"
	}
	var host url.InvalidHostError
	if errors.As(err, &host) {
		return "contains an invalid character in the host name"
	}
	return "cause withheld: naming it would quote the dsn"
}

// describeScheme names the URL scheme of a rejected DSN, for an error that may
// not quote the DSN itself.
//
// Only a prefix that is a scheme by RFC 3986 (`ALPHA *( ALPHA / DIGIT / "+" /
// "-" / "." )`) is reported. The check is not pedantry: the libpq keyword/value
// form has no scheme, and `password=p://x` would hand a naive split the
// password as its "scheme" — the exact leak this function exists to prevent.
func describeScheme(dsn string) string {
	if dsn == "" {
		return "got an empty dsn"
	}
	scheme, _, found := strings.Cut(dsn, "://")
	if !found || !isURLScheme(scheme) {
		return "got no URL scheme"
	}
	return fmt.Sprintf("got scheme %q", scheme)
}

// isURLScheme reports whether s is a syntactically valid URL scheme.
func isURLScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}
