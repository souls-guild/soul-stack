package legion

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// minSidPrefixLen -- lower bound on --sid-prefix length. A short/empty prefix
// in Cleanup's LIKE expression would wipe out other (real) souls. 3 chars is
// the minimal reasonable discriminator of a test legion from the prod fleet.
const minSidPrefixLen = 3

// validateSidPrefix -- single validity guard for the legion prefix (shared by
// the INSERT and Cleanup phases). Protects the real souls registry: with an
// empty/short prefix, Cleanup's `DELETE ... LIKE 'prefix%'` would delete the
// whole fleet. The prefix must be no shorter than minSidPrefixLen and contain
// no whitespace (a typo in the flag).
func validateSidPrefix(sidPrefix string) error {
	if len(sidPrefix) < minSidPrefixLen {
		return fmt.Errorf("legion: refused: prefix %q would wipe out foreign souls (minimum %d chars)", sidPrefix, minSidPrefixLen)
	}
	if strings.ContainsAny(sidPrefix, " \t\r\n") {
		return fmt.Errorf("legion: refused: prefix %q contains whitespace", sidPrefix)
	}
	return nil
}

// escapeLikePrefix escapes LIKE metacharacters (% _ and the escape char \
// itself) in the prefix so it matches literally under `LIKE ... ESCAPE '\'`.
// Without this a prefix like "legion_" would be interpreted as "legion + any
// character".
func escapeLikePrefix(sidPrefix string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(sidPrefix)
}

// Registrar -- setup phase: pre-registers N fake-Soul identities in the
// Keeper cluster's DB (souls + soul_seeds). Keeper authorizes the
// EventStream by fingerprint in soul_seeds (status='active'); without
// pre-registration the stream is rejected as "unknown soul seed"
// (keeper/internal/grpc/auth.go). This is the cheapest valid onboarding path
// for N souls: direct SQL instead of a real Bootstrap-CSR (like
// tests/e2e/harness/cert.go::RegisterSoulPreAuth).
type Registrar struct {
	pool *pgxpool.Pool
}

// NewRegistrar opens a pgx pool on the Keeper cluster's DSN. Caller must call
// Close.
func NewRegistrar(ctx context.Context, dsn string) (*Registrar, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("legion: parse dsn: %w", err)
	}
	// Pool for the setup phase's batch INSERT: not on the stream hot path,
	// keep it modest.
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("legion: pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("legion: pg ping: %w", err)
	}
	return &Registrar{pool: pool}, nil
}

// Close releases the pool.
func (r *Registrar) Close() {
	if r.pool != nil {
		r.pool.Close()
	}
}

// Register inserts souls + soul_seeds for all ids in one transaction via
// pgx.Batch (minimum round-trips for setting up N souls). Idempotent:
// ON CONFLICT reuses the row (a repeat run with the same legion prefix).
// Columns are fixed against the live schema (migrations 007 souls / 009+
// soul_seeds): status 'connected', transport 'agent', seed 'active' with a
// 365-day TTL.
//
// status='connected' (not 'pending') is deliberate: when bringing up an
// EventStream, Keeper does NOT write souls.status to PG (presence is derived
// from the Redis SID-lease, ADR-006(a) amend) -- that field is only touched
// by onboarding-CSR (bootstrap.go). Pre-registering as 'pending' would stay
// 'pending' forever and would be misleading (fleet "not connected" in PG,
// even though the streams are live). The presence authority is the
// Redis-lease and the keeper_grpc_streams_active metric, not this field.
//
// sidPrefix is validated by validateSidPrefix before writing: the same guard
// that protects Cleanup, so we don't create a legion under a dangerous
// prefix.
//
// covens -- stable coven labels for the legion (souls.coven, text[],
// ADR-008). Written for every SID so a Voyage can target the fleet by coven
// (`coven @> $::text[]` in VoyageCommandPGResolver). Empty slice -> coven
// stays ARRAY[]::TEXT[] (schema default), the legion is not targetable by
// coven. All labels are already validated by the caller.
func (r *Registrar) Register(ctx context.Context, sidPrefix string, covens []string, ids []Identity) error {
	if err := validateSidPrefix(sidPrefix); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("legion: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Write a nil slice as an empty text[] (schema default); pgx serializes
	// []string directly into text[] (the same type VoyageCommandPGResolver
	// reads).
	covenArg := covens
	if covenArg == nil {
		covenArg = []string{}
	}

	batch := &pgx.Batch{}
	for _, id := range ids {
		// souls FIRST: soul_seeds.sid is an FK on souls(sid). ON CONFLICT
		// also updates coven: a repeat run with a different --coven
		// rewrites the legion's labels.
		batch.Queue(`
			INSERT INTO souls (sid, status, transport, coven, registered_at, last_seen_at)
			VALUES ($1, 'connected', 'agent', $2::text[], NOW(), NOW())
			ON CONFLICT (sid) DO UPDATE SET status = 'connected', coven = $2::text[], last_seen_at = NOW()
		`, id.SID, covenArg)
	}
	for _, id := range ids {
		batch.Queue(`
			INSERT INTO soul_seeds (sid, fingerprint, serial_number, status, issued_at, expires_at)
			VALUES ($1, $2, $3, 'active', NOW(), NOW() + INTERVAL '365 days')
			ON CONFLICT (fingerprint) DO NOTHING
		`, id.SID, id.Fingerprint, id.Serial)
	}

	br := tx.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("legion: batch exec: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("legion: batch close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("legion: commit: %w", err)
	}
	return nil
}

// Cleanup deletes the legion from the registry by SID prefix (souls will
// cascade-delete soul_seeds via FK ON DELETE CASCADE). Returns the number of
// deleted souls rows. Isolates test SIDs from the real fleet: only cleans up
// legion-* by prefix.
//
// SAFETY: the prefix is validated by validateSidPrefix (empty/short ->
// refuse, DELETE is not executed); LIKE metacharacters (% _ \) are escaped
// and matched literally via ESCAPE '\'. Without this an empty prefix would
// wipe out the ENTIRE souls registry (including the real fleet), and a
// prefix with '_'/'%' would capture foreign SIDs.
func (r *Registrar) Cleanup(ctx context.Context, sidPrefix string) (int64, error) {
	if err := validateSidPrefix(sidPrefix); err != nil {
		return 0, err
	}
	pattern := escapeLikePrefix(sidPrefix) + "%"
	tag, err := r.pool.Exec(ctx, `DELETE FROM souls WHERE sid LIKE $1 ESCAPE '\'`, pattern)
	if err != nil {
		return 0, fmt.Errorf("legion: cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteVoyage removes a single Voyage from PG by exact voyage_id (ULID).
// voyage_targets cascade (FK ON DELETE CASCADE, migration 059); errands are
// a soft link without an FK, short-lived (ttl_at), picked up by
// purge_old_errands. Exact PK match (not prefix) -- the removal only affects
// our load-generated Voyage, other runs are untouched. There is no API
// deletion for a terminal Voyage (DELETE /v1/voyages/{id} only cancels
// pending/scheduled), so cleanup goes through direct SQL, same as
// registration.
func (r *Registrar) DeleteVoyage(ctx context.Context, voyageID string) error {
	if voyageID == "" {
		return fmt.Errorf("legion: empty voyage_id for DeleteVoyage")
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM voyages WHERE voyage_id = $1`, voyageID); err != nil {
		return fmt.Errorf("legion: delete voyage %s: %w", voyageID, err)
	}
	return nil
}
