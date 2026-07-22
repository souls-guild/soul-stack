// Package redis is the Keeper-side wrapper over `github.com/redis/go-redis/v9`.
//
// Purpose: a shared Redis client for a Keeper instance (Reaper's lease-based
// leadership, heartbeat/presence cache, apply-events pub/sub — ADR-006). Lives
// in `keeper/internal/`, not `shared/`, for the same reason as
// `keeper/internal/pg`: the Soul binary must not depend on a Redis client
// directly (ADR-011 / isolation).
//
// Topology (ADR-006 amendment) is chosen via [Config.Mode]:
//   - standalone (default) — a single node;
//   - sentinel — Redis Sentinel HA (master reached through sentinel nodes);
//   - cluster — Redis Cluster (slot-based sharding).
//
// All three implement `redis.UniversalClient`, so sibling files in the
// package (lease/soullease/conclave/herald/applybus/…) work through
// [Client.underlying] uniformly. Cluster-specific logic (per-master SCAN for
// presence, hash-tags on the Herald queue) lives in conclave.go /
// heralddelivery.go where it's needed.
//
// Lease semantics (compare-and-set/delete Lua scripts, ErrLeaseLost) live in
// the sibling file [lease.go]. This file covers connect/ping/close and
// vault-resolving the password.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// Mode constants for Redis topology. Match the `redis.mode` enum in the
// config schema (shared/config/schema.go::enumRedisMode). An empty Mode ==
// ModeStandalone.
const (
	ModeStandalone = "standalone"
	ModeSentinel   = "sentinel"
	ModeCluster    = "cluster"
)

// defaultPasswordField is the Vault KV-secret field holding the plaintext
// Redis password when the vault-ref doesn't specify an explicit `#field`.
// Symmetric with pg.vaultDSNField (fixed at `dsn` there), but here the field
// can be overridden via `#field`.
const defaultPasswordField = "password"

// Config holds the connection parameters. Corresponds to the
// `keeper.yml::redis` block (see shared/config/keeper.go::KeeperRedis).
//
// `Mode` is the topology (see the constants above); empty = standalone
// (forward-compat). `Addr` is the node address for standalone.
// `Sentinels`/`MasterName` are for sentinel. `Nodes` is for cluster.
//
// `PasswordRef` / `SentinelPasswordRef` — the Redis password / sentinel-node
// password, either a vault-ref `vault:<mount>/<path>[#field]` (resolved via
// [resolvePassword]) or plaintext (tests against a password-protected Redis
// without a Vault fixture).
type Config struct {
	Mode                string
	Addr                string
	PasswordRef         string
	MasterName          string
	Sentinels           []string
	Nodes               []string
	SentinelPasswordRef string
	DB                  int
}

// passwordResolver is the narrow contract for vault password resolution
// (what [resolvePassword] uses). `*keepervault.Client` satisfies it; the
// interface exists for testability (a stub without a live Vault) and mirrors
// pg.ResolveDSN, which takes a concrete `*keepervault.Client`.
type passwordResolver interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// ErrVaultClientRequired is returned when `password_ref`/`sentinel_password_ref`
// starts with `vault:` but no vault-client was passed (nil). Caller invariant
// (`keeper/cmd/keeper`): the vault-client is brought up before setupRedis.
// Parity with pg.ErrVaultClientRequired.
var ErrVaultClientRequired = errors.New("redis: vault client is required for vault:-ref")

// ErrPasswordFieldMissing is returned when the password field (`password` or
// a `#field` override) is missing, empty, or not a string in the Vault KV.
var ErrPasswordFieldMissing = errors.New("redis: password field missing or empty in Vault KV")

// Client is a thin wrapper over `redis.UniversalClient` (standalone/sentinel/
// cluster — `*redis.Client` / `*redis.FailoverClient` / `*redis.ClusterClient`).
// No extra logic: the constructor hides the go-redis dependency from other
// keeper packages and gives a single point for future OTel tracing.
type Client struct {
	rdb redis.UniversalClient
}

// NewClient builds a client per [Config.Mode] and does one Ping, so that
// dependent subsystems fail fast at startup when Redis is unreachable,
// instead of on the first operation.
//
// `vc` (vault-client) is required only if `password_ref`/`sentinel_password_ref`
// is a vault-ref (`vault:...`); pass nil for plaintext/empty. Parity with
// pg.NewPool(ctx, cfg, vc).
func NewClient(ctx context.Context, cfg Config, vc passwordResolver) (*Client, error) {
	password, err := resolvePassword(ctx, vc, cfg.PasswordRef)
	if err != nil {
		return nil, fmt.Errorf("redis: resolve password: %w", err)
	}
	sentinelPassword, err := resolvePassword(ctx, vc, cfg.SentinelPasswordRef)
	if err != nil {
		return nil, fmt.Errorf("redis: resolve sentinel password: %w", err)
	}

	rdb, err := build(cfg, password, sentinelPassword)
	if err != nil {
		return nil, err
	}

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping (mode=%s): %w", resolvedMode(cfg.Mode), err)
	}

	return &Client{rdb: rdb}, nil
}

// build picks the concrete go-redis implementation per [Config.Mode]. A plain
// switch on mode: passwords are already resolved by the caller ([NewClient]),
// no vault dependency here. Ping lives in [NewClient].
func build(cfg Config, password, sentinelPassword string) (redis.UniversalClient, error) {
	switch resolvedMode(cfg.Mode) {
	case ModeStandalone:
		if cfg.Addr == "" {
			return nil, errors.New("redis: addr is empty (mode=standalone)")
		}
		return redis.NewClient(&redis.Options{
			Addr:     cfg.Addr,
			Password: password,
			DB:       cfg.DB,
		}), nil

	case ModeSentinel:
		if cfg.MasterName == "" {
			return nil, errors.New("redis: master_name is empty (mode=sentinel)")
		}
		if len(cfg.Sentinels) == 0 {
			return nil, errors.New("redis: sentinels is empty (mode=sentinel)")
		}
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.MasterName,
			SentinelAddrs:    cfg.Sentinels,
			Password:         password,
			SentinelPassword: sentinelPassword,
			DB:               cfg.DB,
		}), nil

	case ModeCluster:
		if len(cfg.Nodes) == 0 {
			return nil, errors.New("redis: nodes is empty (mode=cluster)")
		}
		// DB doesn't apply in cluster mode (go-redis ClusterOptions has no
		// DB — Redis Cluster has no logical databases, only slot 0).
		// Silently ignored: the config schema doesn't forbid DB, but
		// cluster always operates on db0.
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    cfg.Nodes,
			Password: password,
		}), nil

	default:
		return nil, fmt.Errorf("redis: unknown mode %q", cfg.Mode)
	}
}

// resolvedMode normalizes an empty Mode to ModeStandalone (forward-compat).
func resolvedMode(mode string) string {
	if mode == "" {
		return ModeStandalone
	}
	return mode
}

// Ping is a health-check for the Reaper loop / readiness probes.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close closes the underlying client. Idempotent (a repeat Close on
// go-redis/v9 returns [redis.ErrClosed] — we ignore it).
func (c *Client) Close() error {
	err := c.rdb.Close()
	if err == nil || errors.Is(err, redis.ErrClosed) {
		return nil
	}
	return err
}

// underlying gives sibling files in the package
// (lease/soullease/conclave/herald/applybus/…) access to the go-redis client.
// Not exposed to external packages. The type is the UniversalClient
// interface: all three topologies are hidden behind it. Sibling files reach
// cluster-specific functionality (ForEachMaster) via a type-switch on the
// concrete type.
func (c *Client) underlying() redis.UniversalClient { return c.rdb }

// resolvePassword turns a config password-ref into a plaintext password.
// Supports:
//
//   - an empty string → "" (connect without a password: dev stack, docker-compose);
//   - a non-`vault:` string → as-is (plaintext: unit/integration tests against
//     a password-protected Redis without a Vault fixture);
//   - a vault-ref `vault:<mount>/<path>[#field]` → reads the field from Vault
//     KV (`#field` override, default `password`). `vc` is required, or
//     [ErrVaultClientRequired].
//
// Symmetric with pg.ResolveDSN, but with a `#field` override (pg has a fixed
// `dsn` field).
func resolvePassword(ctx context.Context, vc passwordResolver, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if !strings.HasPrefix(ref, "vault:") {
		return ref, nil
	}
	if vc == nil {
		return "", fmt.Errorf("%w: ref=%q", ErrVaultClientRequired, ref)
	}

	// Strip the `#field` override BEFORE ParseRef: the vault-ref form is
	// `vault:<m>/<p>#field`, the field comes after '#'. ParseRef validates and
	// normalizes the logical path without the field.
	refPath, field := splitFieldOverride(ref)
	path, err := keepervault.ParseRef(refPath)
	if err != nil {
		return "", fmt.Errorf("redis: password_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("redis: read vault %q: %w", path, err)
	}
	return extractPassword(kv, field)
}

// splitFieldOverride strips the `#field` suffix from a vault-ref. Without a
// '#' it returns (ref, defaultPasswordField).
func splitFieldOverride(ref string) (refWithoutField, field string) {
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, defaultPasswordField
}

// extractPassword pulls the `field` value out of the Vault KV payload (string).
func extractPassword(kv map[string]any, field string) (string, error) {
	raw, ok := kv[field]
	if !ok {
		return "", fmt.Errorf("%w: field=%q", ErrPasswordFieldMissing, field)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("redis: vault field %q has unsupported type %T (want string)", field, raw)
	}
	if s == "" {
		return "", fmt.Errorf("%w: field=%q", ErrPasswordFieldMissing, field)
	}
	return s, nil
}
