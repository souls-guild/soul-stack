// Package pg provides pgx-pool initialization for Keeper under ADR-005
// (Postgres is the only cold storage for state).
//
// Pool ownership resides in `keeper/cmd/keeper` (M0.4.2); each subsystem
// needing the database (`shared/audit`, souls/operators registries, Reaper)
// receives an already-initialized `*pgxpool.Pool` via constructor — the
// package does not hold global state.
package pg

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// vaultDSNField is the field name inside Vault KV-secret containing the
// plain DSN for Postgres. Matches the convention in [docs/keeper/config.md]
// and supplement [docs/dev/local-setup.md] (`vault kv put secret/keeper/postgres
// dsn="postgres://..."`).
const vaultDSNField = "dsn"

// ErrEmptyDSN is returned if `cfg.DSNRef` is empty. Callers can use
// `errors.Is(err, pg.ErrEmptyDSN)` for classification (e.g., to distinguish
// "config incomplete" from "vault unavailable").
var ErrEmptyDSN = errors.New("pg: empty dsn_ref")

// ErrVaultClientRequired is returned when `dsn_ref` starts with `vault:`
// but no vault-client was provided. This is a caller invariant
// (`keeper/cmd/keeper`): vault-client is always initialized before NewPool.
var ErrVaultClientRequired = errors.New("pg: vault client is required for vault:-ref")

// ErrDSNFieldMissing is returned if the Vault KV field specified by
// [vaultDSNField] is missing, empty, or has an unsupported type.
var ErrDSNFieldMissing = fmt.Errorf("pg: %q field missing or empty in Vault KV", vaultDSNField)

// ErrMalformedDSN reports a DSN pgx refuses to parse. It carries pgx's
// diagnosis, or a class of fault, but never the DSN or any slice of it.
//
// A DSN is a credential, and this is the first refusal on keeper startup to
// hold one: it reaches stderr as `keeper run: pg pool: …`, which in a cluster
// deployment is a pod log shipped to central collection (NIM-418).
//
// The pgx error is therefore never wrapped. [pgconn.ParseConfigError] quotes
// the whole connection string and redacts it with a regex over `password=`,
// which misses the libpq-legal spellings `password = x` and `password= x` —
// those reach the log verbatim, in every branch, including the ones that fail
// on an unrelated setting. And when net/url is what refused, pgx returns that
// error with its wrapper stripped, so a `#`, `/` or `?` in a password becomes
// `invalid port ":<password>" after host`. pgx says as much in a comment on
// `ParseConfigError.Error`: a static string would be the safe thing to return.
var ErrMalformedDSN = errors.New("pg: dsn cannot be parsed")

// withheldCause stands in for a diagnosis that cannot be given without
// quoting the DSN. An operator reading it knows the fault is in the DSN's
// syntax and that the text is being kept out of the log on purpose.
const withheldCause = "cause withheld: naming it would quote the dsn"

// NewPool opens a connection pool to Postgres using `cfg.DSNRef` / `cfg.Pool`.
// The pool does not ping the database — that is a separate [Ping] call on
// keeper startup after all dependencies are initialized.
//
// `vc` (vault-client) is required only if `cfg.DSNRef` is a vault-ref
// (`vault:<mount>/<path>`); nil can be passed for plain-DSN.
//
// Pool min/max are taken from `cfg.Pool.Min` / `cfg.Pool.Max`. If both
// are zero (fixtures without an explicit pool block) — pgx uses its own
// defaults (max=4); this behavior is accepted to avoid duplicating the
// default pool-config between config-schema and code.
func NewPool(ctx context.Context, cfg config.KeeperPostgres, vc *keepervault.Client) (*pgxpool.Pool, error) {
	dsn, err := ResolveDSN(ctx, vc, cfg.DSNRef)
	if err != nil {
		return nil, err
	}
	pcfg, err := parsePoolConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.Pool.Max > 0 {
		pcfg.MaxConns = int32(cfg.Pool.Max)
	}
	if cfg.Pool.Min > 0 {
		pcfg.MinConns = int32(cfg.Pool.Min)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("pg: new pool: %w", err)
	}
	return pool, nil
}

// parsePoolConfig is [pgxpool.ParseConfig] with the DSN kept out of the error.
// Failures are [ErrMalformedDSN]; see the note there for why pgx's own message
// cannot be passed on.
//
// A URL-form DSN goes through net/url here first. That is not a second opinion
// on pgx: pgx runs the very same parser and hands back its error unwrapped, so
// the only way to keep a password out of the message is to reach the failure
// before pgx describes it. Nothing net/url accepts is refused here, and pgx
// accepts nothing net/url rejects, so this cannot lock out a DSN the keeper
// otherwise runs on — multi-host, IPv6 with a zone, `?host=/var/run/postgresql`
// and percent-encoded or empty passwords all parse.
func parsePoolConfig(dsn string) (*pgxpool.Config, error) {
	// Both spellings of a password in a URL, collected for the containment
	// check in describeParseConfigFailure. A keyword/value DSN has no
	// equivalent — pgx's parser for that form is unexported, and every cause it
	// reports is a constant.
	var secrets []string
	if isURLDSN(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, fmt.Errorf("%w (%s)", ErrMalformedDSN, describeURLFailure(err))
		}
		if password, ok := u.User.Password(); ok {
			secrets = append(secrets, password)
		}
		if password := u.Query().Get("password"); password != "" {
			secrets = append(secrets, password)
		}
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", ErrMalformedDSN, describeParseConfigFailure(err, secrets))
	}
	return cfg, nil
}

// isURLDSN reports whether pgx reads dsn as a URL rather than as libpq
// keyword/value. The two prefixes are pgconn.ParseConfig's own dispatch.
func isURLDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// describeURLFailure names the class of a net/url failure without quoting the
// text that failed.
//
// Type and value disclose differently, and that gap is the whole function:
// [url.EscapeError]'s TYPE says "there is a bare % in here", while its VALUE is
// three characters of the password. Every branch returns a constant, so no
// input reaches the output.
//
// `keeper/internal/migrate` classifies the same failures for the same reason
// and keeps its own copy: each package proves its own guarantee in its own
// guard test, which is what catches the two drifting apart.
func describeURLFailure(err error) string {
	var escape url.EscapeError
	if errors.As(err, &escape) {
		return "contains an invalid percent-escape"
	}
	var host url.InvalidHostError
	if errors.As(err, &host) {
		return "contains an invalid character in the host name"
	}
	return withheldCause
}

// describeParseConfigFailure recovers pgx's diagnosis without the connection
// string pgx wraps it in.
//
// `ConnString` is an exported field, so blanking it on a copy leaves pgx to
// format the rest — which keeps `sslmode is invalid` and `cannot parse
// pool_max_conns` in the operator's hands instead of trading the whole
// diagnosis away for safety. A keeper that refuses to start over a DSN the
// operator cannot read from Vault is a bad enough place to be.
//
// The containment check is the belt to that braces. Every cause pgx reports
// today names a setting, a host or a file path — but that is a fact about one
// version of a dependency, not a guarantee, and this package has already been
// wrong once about how well pgx redacts. A password short enough to occur by
// chance (`5432`) costs a diagnosis; the other way round costs a credential.
func describeParseConfigFailure(err error, secrets []string) string {
	desc := err.Error()
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		safe := *parseErr
		safe.ConnString = ""
		desc = strings.TrimPrefix(safe.Error(), "cannot parse ``: ")
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(desc, secret) {
			return withheldCause
		}
	}
	return desc
}

// ResolveDSN converts a `dsn_ref` from config into a plain-DSN. Supports:
//
//   - plain DSN: `postgres://...` / `postgresql://...` — returned as-is
//     (ctx/vc ignored).
//   - vault-ref: `vault:<mount>/<path>` — reads the `dsn` field from Vault KV.
//     `vc` is required; otherwise [ErrVaultClientRequired].
//
// Empty `ref` → [ErrEmptyDSN]; missing/empty `dsn` field in Vault →
// [ErrDSNFieldMissing]; field not a string → error.
//
// Exported so `keeper/internal/migrate.Apply` can get the same DSN as
// [NewPool] without duplicating vault-resolve logic.
func ResolveDSN(ctx context.Context, vc *keepervault.Client, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("%w", ErrEmptyDSN)
	}
	if !strings.HasPrefix(ref, "vault:") {
		return ref, nil
	}
	if vc == nil {
		return "", fmt.Errorf("%w: ref=%q", ErrVaultClientRequired, ref)
	}
	path, err := keepervault.ParseRef(ref)
	if err != nil {
		return "", fmt.Errorf("pg: dsn_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("pg: read vault %q: %w", path, err)
	}
	return extractDSN(kv)
}

// extractDSN extracts the `dsn` field from a Vault KV payload. PM-decision C:
// DSN is always a plain string, without base64-fallback (which remains in
// bootstrap.extractSigningKey for binary key specifics).
func extractDSN(kv map[string]any) (string, error) {
	raw, ok := kv[vaultDSNField]
	if !ok {
		return "", ErrDSNFieldMissing
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("pg: vault %q has unsupported type %T (want string)", vaultDSNField, raw)
	}
	if s == "" {
		return "", ErrDSNFieldMissing
	}
	return s, nil
}
