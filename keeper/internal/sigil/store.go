package sigil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgErrCodeUniqueViolation is SQLSTATE for UNIQUE violations: PK or partial unique
// index plugin_sigils_active_idx (active record on (namespace, name, ref) already
// exists). Held locally like operator/applyrun CRUD.
const pgErrCodeUniqueViolation = "23505"

// pgErrCodeForeignKeyViolation is SQLSTATE for FK violations. For plugin_sigils
// occurs on allowed_by_aid / revoked_by_aid (reference to non-existent AID).
const pgErrCodeForeignKeyViolation = "23503"

// ErrSigilAlreadyActive is returned by Insert when an active record already exists
// on (namespace, name, ref) (partial unique index). Re-allow requires first
// revoking the current active record.
var ErrSigilAlreadyActive = errors.New("sigil: an active record already exists for (namespace, name, ref)")

// ErrSigilNotFound is returned by GetActive / Revoke when no active record is found
// by key.
var ErrSigilNotFound = errors.New("sigil: no active record found")

// Sigil is a row in the plugin_sigils registry (migrations 028, 030, 038).
//
// ManifestRaw is byte-exact RAW bytes of manifest.yaml over which the signature was
// placed (migration 030). This is the CANON for verify/broadcast: sent to
// PluginSigil.manifest as-is, S6 re-hashes exactly these via NormalizeManifestBytes
// (S3↔S6 invariant).
//
// Manifest is stored as JSONB FOR query/audit (search by side_effects/capabilities,
// show in UI). This is a DERIVED projection, NOT the canon for verify: JSONB
// round-trip does not preserve bytes.
// Signature is raw bytes of ed25519 signature (BYTEA, no base64).
//
// CommitSHA is the git commit from which the granted binary was resolved (migration 038,
// ADR-026(g)). Audit ORIGIN marker, OUTSIDE the signed block: integrity authority is
// SHA256 + Keeper signature, not CommitSHA. Keeper-audit-only: NOT used in verify
// (absent from shared/pluginhost.SigilRecord) and NOT sent in PluginSigil broadcast
// transport. Filled from ResolvedSlot.CommitSHA on git-verified-allow (S4 slice);
// empty string = legacy operator-asserted (Variant C) or pre-038 row (NULL read as "").
type Sigil struct {
	ID           int64
	Namespace    string
	Name         string
	Ref          string
	SHA256       string // hex, lowercase, 64 chars
	Signature    []byte // raw 64 bytes ed25519
	ManifestRaw  []byte // byte-exact raw bytes of manifest.yaml (CANON for verify)
	Manifest     []byte // JSONB bytes (derived projection for query/audit, not CANON)
	CommitSHA    string // git-commit origin (audit, outside signature); "" = legacy/NULL
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
	RevokedByAID *string
}

// ExecQueryRower is a narrow subset of pgxpool.Pool needed by CRUD. The narrowing
// allows unit testing via fake-pool; real pool satisfies automatically. Symmetric
// to operator/applyrun CRUD.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// insertSigilSQL writes commit_sha as NULLIF($9, “”): empty CommitSHA
// (legacy operator-asserted / resolver fill comes in S4) is stored as NULL in DB,
// preserving semantics of “origin unknown” rather than empty string.
const insertSigilSQL = `
INSERT INTO plugin_sigils (namespace, name, ref, sha256, signature, manifest, allowed_by_aid, manifest_raw, commit_sha)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
RETURNING id, allowed_at
`

// selectActiveByKeySQL / listActiveSQL reads commit_sha via
// COALESCE(..., “”) (NULL → “” for legacy/pre-038 rows), so scanning into a string
// field does not require a pointer.
const selectActiveByKeySQL = `
SELECT id, namespace, name, ref, sha256, signature, manifest, manifest_raw,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE namespace = $1 AND name = $2 AND ref = $3 AND revoked_at IS NULL
`

const listActiveSQL = `
SELECT id, namespace, name, ref, sha256, signature, manifest, manifest_raw,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE revoked_at IS NULL
ORDER BY allowed_at DESC, id DESC
`

// revokeActiveByKeySQL is soft revocation of active record by key. WHERE
// revoked_at IS NULL is atomic protection against repeated revoke (rows-affected = 0
// → no active record).
const revokeActiveByKeySQL = `
UPDATE plugin_sigils
SET revoked_at = NOW(), revoked_by_aid = $4
WHERE namespace = $1 AND name = $2 AND ref = $3 AND revoked_at IS NULL
`

// Insert inserts a new grant record (allow). ManifestRaw is byte-exact raw bytes
// of manifest.yaml (CANON for verify), manifest is JSONB projection, signature is
// raw bytes of ed25519 signature. CommitSHA is audit origin marker (outside signature);
// empty is allowed (NULL = legacy/unknown, resolver fill is S4). id and allowed_at
// are populated from DB (RETURNING).
//
// Re-allow after Revoke is a clean Insert of a new record: partial unique index
// plugin_sigils_active_idx counts only active rows, revoked ones do not interfere.
// If an active record already exists → [ErrSigilAlreadyActive].
func Insert(ctx context.Context, db ExecQueryRower, s *Sigil) error {
	if s == nil {
		return fmt.Errorf("sigil: nil sigil")
	}
	if !reSHA256Hex.MatchString(s.SHA256) {
		return fmt.Errorf("sigil: sha256 %q must be 64 lower-hex chars", s.SHA256)
	}
	if len(s.Signature) == 0 {
		return fmt.Errorf("sigil: signature is empty")
	}
	if s.AllowedByAID == "" {
		return fmt.Errorf("sigil: allowed_by_aid is empty")
	}
	// Empty ManifestRaw is a caller bug (root of trust): signature is placed over
	// EXACTLY these bytes, no fallback possible (Normalize("{}") !=
	// Normalize(""), unlike JSONB column manifest which allows "{}"-stub).
	if len(s.ManifestRaw) == 0 {
		return fmt.Errorf("sigil: manifest_raw is empty (signed bytes must be persisted byte-exact)")
	}
	manifest := s.Manifest
	if len(manifest) == 0 {
		manifest = []byte("{}")
	}

	err := db.QueryRow(ctx, insertSigilSQL,
		s.Namespace, s.Name, s.Ref, s.SHA256, s.Signature, manifest, s.AllowedByAID, s.ManifestRaw, s.CommitSHA,
	).Scan(&s.ID, &s.AllowedAt)
	if err != nil {
		return mapInsertError(err)
	}
	return nil
}

// mapInsertError maps pgx errors from Insert to package sentinels.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrSigilAlreadyActive, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("sigil: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("sigil: insert: %w", err)
}

// GetActive reads an active (non-revoked) record by key (namespace, name, ref).
// Lookup path for future S6-verify. Returns [ErrSigilNotFound] if no active record
// exists.
func GetActive(ctx context.Context, db ExecQueryRower, namespace, name, ref string) (*Sigil, error) {
	row := db.QueryRow(ctx, selectActiveByKeySQL, namespace, name, ref)
	s, err := scanSigil(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSigilNotFound
		}
		return nil, err
	}
	return s, nil
}

// ListActive returns all active records, newest first. Feed of allow-list for UI /
// audit triage.
func ListActive(ctx context.Context, db ExecQueryRower) ([]*Sigil, error) {
	rows, err := db.Query(ctx, listActiveSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list active: %w", err)
	}
	defer rows.Close()

	var out []*Sigil
	for rows.Next() {
		s, err := scanSigil(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list active rows: %w", err)
	}
	return out, nil
}

// Revoke soft revokes an active grant by key: sets revoked_at = NOW() and
// revoked_by_aid. Record remains in registry for audit.
//
// Semantics:
//   - no active record by key → [ErrSigilNotFound];
//   - revokedByAID is empty → error (audit invariant: who revoked is required).
func Revoke(ctx context.Context, db ExecQueryRower, namespace, name, ref, revokedByAID string) error {
	if revokedByAID == "" {
		return fmt.Errorf("sigil: revoked_by_aid is empty")
	}
	tag, err := db.Exec(ctx, revokeActiveByKeySQL, namespace, name, ref, revokedByAID)
	if err != nil {
		return fmt.Errorf("sigil: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSigilNotFound
	}
	return nil
}

// scanSigil is common Scan of one plugin_sigils row. Extracted so GetActive
// and ListActive read columns identically.
func scanSigil(row pgx.Row) (*Sigil, error) {
	var s Sigil
	err := row.Scan(
		&s.ID,
		&s.Namespace,
		&s.Name,
		&s.Ref,
		&s.SHA256,
		&s.Signature,
		&s.Manifest,
		&s.ManifestRaw,
		&s.CommitSHA,
		&s.AllowedByAID,
		&s.AllowedAt,
		&s.RevokedAt,
		&s.RevokedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("sigil: scan: %w", err)
	}
	return &s, nil
}
