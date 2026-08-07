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

// pgErrCodeUniqueViolation is SQLSTATE for UNIQUE violations: PK or one of the two
// partial unique indexes of plugin_sigils (see [mapInsertError]). Held locally like
// operator/applyrun CRUD.
const pgErrCodeUniqueViolation = "23505"

// pgErrCodeForeignKeyViolation is SQLSTATE for FK violations. For plugin_sigils
// occurs on allowed_by_aid / revoked_by_aid (reference to non-existent AID).
const pgErrCodeForeignKeyViolation = "23503"

// activeSourceRefIdx / activeAliasIdx — the two partial unique indexes of
// plugin_sigils (migration 113). Named here so [mapInsertError] can tell the two
// collisions apart: they mean different things to an operator and have different
// fixes.
const (
	activeSourceRefIdx = "plugin_sigils_active_idx"
	activeAliasIdx     = "plugin_sigils_active_alias_idx"
)

// ErrSigilAlreadyActive is returned by Insert when an active grant already exists for
// this (source, ref) — the TRUST key. Re-approving the same artifact identity requires
// revoking the current grant first, so that an approval is never silently duplicated.
var ErrSigilAlreadyActive = errors.New("sigil: an active grant already exists for (source, ref)")

// ErrAliasAlreadyRegistered is returned by Insert when the alias is already taken by
// another active grant.
//
// This is a REGISTRATION conflict, not a trust one: one alias is one address space and
// one host slot, so two live grants under one alias would leave `<alias>.<module>`
// meaning two different sets of bytes, and the runtime lookup picking whichever row
// came back first.
var ErrAliasAlreadyRegistered = errors.New("sigil: the alias is already registered by an active grant")

// ErrSigilNotFound is returned by GetActive / Revoke when no active record is found
// by key.
var ErrSigilNotFound = errors.New("sigil: no active record found")

// Sigil is a row in the plugin_sigils registry (migrations 028, 038, 113).
//
// # Two identities, and the split is the point (NIM-377 / NIM-438)
//
// Source + Ref are what the grant is ON and what the signature covers. The artifact
// carries no self-name, so where it came from is the only identity a signature can be
// over — and keying trust there is what stops a rename from walking around an approved
// hash: an alias is operator-chosen text, and trust anchored to operator-chosen text is
// trust the operator can move.
//
// Alias is the operator's registration — address level 1 and the runtime LOOKUP key.
// It is NOT in the signed block, so re-registering the same bytes under a second alias
// needs no second signature. At most one ACTIVE grant may hold a given alias
// (plugin_sigils_active_alias_idx), which is what keeps the lookup single-valued.
//
// Schema is the byte-exact canonical schema document the signature was placed over.
// This is the CANON for verify/broadcast: it rides PluginSigil.schema as-is and S6
// re-hashes exactly these bytes via SchemaDigest (S3↔S6 invariant). One column, not
// two: the document is already canonical JSON, so a derived JSONB projection would only
// be a second version of the same bytes that could disagree with the signed one.
//
// Signature is the raw ed25519 signature (BYTEA, no base64).
//
// CommitSHA is the git commit the granted artifact was resolved from (ADR-026(g)).
// Audit ORIGIN marker, OUTSIDE the signed block: the integrity authority is SHA256 +
// the Keeper signature, not CommitSHA. Keeper-audit-only — absent from
// shared/pluginhost.SigilRecord and not sent in the PluginSigil broadcast. Empty
// string = origin unknown (stored as NULL).
type Sigil struct {
	ID           int64
	Alias        string
	Source       string
	Ref          string
	SHA256       string // hex, lowercase, 64 chars
	Signature    []byte // raw 64 bytes ed25519
	Schema       []byte // byte-exact canonical schema document (CANON for verify)
	CommitSHA    string // git-commit origin (audit, outside signature); "" = unknown/NULL
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

// insertSigilSQL writes commit_sha through NULLIF: an empty CommitSHA is stored as
// NULL, preserving the semantics of "origin unknown" rather than an empty string.
const insertSigilSQL = `
INSERT INTO plugin_sigils (alias, source, ref, sha256, signature, schema, commit_sha, allowed_by_aid)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
RETURNING id, allowed_at
`

// selectActiveByAliasSQL / listActiveSQL read commit_sha through COALESCE (NULL becomes
// an empty string for rows with unknown origin), so scanning into a string field needs
// no pointer.
//
// The lookup key is the ALIAS: it is what a host slot is named by and the only thing a
// verify path holds when it needs a grant. Uniqueness among active rows makes the
// single-row read exact rather than "first of possibly several".
const selectActiveByAliasSQL = `
SELECT id, alias, source, ref, sha256, signature, schema,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE alias = $1 AND revoked_at IS NULL
`

const listActiveSQL = `
SELECT id, alias, source, ref, sha256, signature, schema,
       COALESCE(commit_sha, ''), allowed_by_aid, allowed_at, revoked_at, revoked_by_aid
FROM plugin_sigils
WHERE revoked_at IS NULL
ORDER BY allowed_at DESC, id DESC
`

// revokeActiveByAliasSQL is the soft revocation of the active grant under an alias.
// WHERE revoked_at IS NULL is the atomic guard against a repeated revoke
// (rows-affected = 0 → there was no active grant).
const revokeActiveByAliasSQL = `
UPDATE plugin_sigils
SET revoked_at = NOW(), revoked_by_aid = $2
WHERE alias = $1 AND revoked_at IS NULL
`

// Insert inserts a new grant record (allow). Schema is the byte-exact canonical schema
// document (the CANON for verify), signature the raw ed25519 bytes. CommitSHA is the
// audit origin marker (outside the signature); empty is allowed (NULL = unknown). id
// and allowed_at come back from the DB (RETURNING).
//
// Re-allow after Revoke is a clean Insert of a new row: both partial unique indexes
// count only active rows, so revoked history never gets in the way. A live collision
// is reported as whichever key it hit — [ErrSigilAlreadyActive] for (source, ref),
// [ErrAliasAlreadyRegistered] for the alias.
func Insert(ctx context.Context, db ExecQueryRower, s *Sigil) error {
	if s == nil {
		return fmt.Errorf("sigil: nil sigil")
	}
	if s.Alias == "" {
		return fmt.Errorf("sigil: alias is empty")
	}
	if s.Source == "" {
		return fmt.Errorf("sigil: source is empty (the artifact's only signed identity)")
	}
	if s.Ref == "" {
		return fmt.Errorf("sigil: ref is empty")
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
	// An empty Schema is a caller bug at the root of trust: the signature was placed
	// over EXACTLY these bytes, and there is no fallback that could reconstruct them.
	if len(s.Schema) == 0 {
		return fmt.Errorf("sigil: schema is empty (the signed bytes must be persisted byte-exact)")
	}

	err := db.QueryRow(ctx, insertSigilSQL,
		s.Alias, s.Source, s.Ref, s.SHA256, s.Signature, s.Schema, s.CommitSHA, s.AllowedByAID,
	).Scan(&s.ID, &s.AllowedAt)
	if err != nil {
		return mapInsertError(err)
	}
	return nil
}

// mapInsertError maps pgx errors from Insert to package sentinels. The two partial
// unique indexes are distinguished by name: hitting the alias index is a registration
// conflict the operator fixes by picking another alias, hitting the (source, ref) index
// is a duplicate approval they fix by revoking first.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			if pgErr.ConstraintName == activeAliasIdx {
				return fmt.Errorf("%w (constraint %s): %w", ErrAliasAlreadyRegistered, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("%w (constraint %s): %w", ErrSigilAlreadyActive, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("sigil: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("sigil: insert: %w", err)
}

// GetActive reads the active (non-revoked) grant registered under alias — the lookup
// the verify path takes. Returns [ErrSigilNotFound] when there is none.
func GetActive(ctx context.Context, db ExecQueryRower, alias string) (*Sigil, error) {
	row := db.QueryRow(ctx, selectActiveByAliasSQL, alias)
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

// Revoke soft-revokes the active grant registered under alias: sets revoked_at = NOW()
// and revoked_by_aid. The row stays in the registry for audit.
//
// The alias is the right handle for the operator gesture — "un-register this" — and it
// identifies exactly one live row. The revoked row keeps its (source, ref), so the
// audit trail still says which artifact identity was approved and by whom.
//
// Semantics:
//   - no active grant under the alias → [ErrSigilNotFound];
//   - revokedByAID empty → error (audit invariant: who revoked is required).
func Revoke(ctx context.Context, db ExecQueryRower, alias, revokedByAID string) error {
	if alias == "" {
		return fmt.Errorf("sigil: alias is empty")
	}
	if revokedByAID == "" {
		return fmt.Errorf("sigil: revoked_by_aid is empty")
	}
	tag, err := db.Exec(ctx, revokeActiveByAliasSQL, alias, revokedByAID)
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
		&s.Alias,
		&s.Source,
		&s.Ref,
		&s.SHA256,
		&s.Signature,
		&s.Schema,
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
