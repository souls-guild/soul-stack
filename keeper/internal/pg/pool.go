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
	"strings"

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
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
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
