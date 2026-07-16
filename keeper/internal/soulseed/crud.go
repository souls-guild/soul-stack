package soulseed

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors for the CRUD layer.
var (
	// ErrSeedActiveExists is an attempt to insert a second active seed for the
	// same SID (partial unique `soul_seeds_active_by_sid_idx`). Caller
	// (bootstrap handler) must first Supersede the old one, then Insert the new
	// one in one transaction.
	ErrSeedActiveExists = errors.New("soulseed: active seed for SID already exists (call SupersedeBySID first)")

	// ErrSeedFingerprintCollision means fingerprint already exists in registry.
	// De facto impossible (public-key SHA-256 is unique), but we keep the
	// constraint explicit. It maps to 500 internal (reissuing the same key is a
	// Vault PKI / CSR-handling bug).
	ErrSeedFingerprintCollision = errors.New("soulseed: fingerprint already exists in registry")

	// ErrSeedSoulNotFound means INSERT references a missing SID in souls.
	ErrSeedSoulNotFound = errors.New("soulseed: target SID not found in souls registry")

	// ErrSeedNotFound means SelectActiveBySID did not find an active seed.
	ErrSeedNotFound = errors.New("soulseed: no active seed for SID")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower is a narrow subset of the pgxpool.Pool interface.
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

const insertSQL = `
INSERT INTO soul_seeds (
    sid, fingerprint, serial_number,
    issued_at, expires_at, issued_by_kid, status
) VALUES ($1, $2, $3, COALESCE($4, NOW()), $5, $6, $7)
RETURNING seed_id, issued_at
`

const selectActiveBySIDSQL = `
SELECT seed_id, sid, fingerprint, serial_number,
       issued_at, expires_at, issued_by_kid, status, revocation_reason
FROM soul_seeds
WHERE sid = $1 AND status = 'active'
`

const selectByFingerprintSQL = `
SELECT seed_id, sid, fingerprint, serial_number,
       issued_at, expires_at, issued_by_kid, status, revocation_reason
FROM soul_seeds
WHERE fingerprint = $1
`

// supersedeBySIDSQL moves all active seeds for this SID to superseded. With the
// normal `_active_by_sid_idx` invariant it touches exactly one row (or zero when
// a new Soul has no active seed yet).
const supersedeBySIDSQL = `
UPDATE soul_seeds
SET status = 'superseded'
WHERE sid = $1 AND status = 'active'
`

// revokeSQL revokes a specific seed (by seed_id). It is used by operator
// revocation API. Old superseded/expired rows are not touched; real protection
// from a revoked client lives at mTLS level (CRL) + WHERE status='active'.
const revokeSQL = `
UPDATE soul_seeds
SET status = 'revoked',
    revocation_reason = $2
WHERE seed_id = $1 AND status IN ('active', 'superseded')
`

// Insert records a new SoulSeed. It MUST run inside a transaction together with
// [SupersedeBySID] (if there was an old active seed), [soul.UpdateStatus], and
// [bootstraptoken.Burn].
//
// Pre-conditions:
//   - s.SID is non-empty;
//   - s.Fingerprint is 64 lower-hex (ValidFingerprintFormat);
//   - s.SerialNumber is non-empty;
//   - s.ExpiresAt > s.IssuedAt (if IssuedAt is not zero; otherwise PG uses NOW()).
//
// Returns:
//   - [ErrSeedActiveExists] on UNIQUE over `soul_seeds_active_by_sid_idx`.
//   - [ErrSeedFingerprintCollision] on UNIQUE over `soul_seeds_fingerprint_idx`.
//   - [ErrSeedSoulNotFound] on FK violation over `soul_seeds_sid_fk`.
func Insert(ctx context.Context, db ExecQueryRower, s *SoulSeed) error {
	if s == nil {
		return fmt.Errorf("soulseed: nil soul_seed")
	}
	if s.SID == "" {
		return fmt.Errorf("soulseed: sid is empty")
	}
	if !ValidFingerprintFormat(s.Fingerprint) {
		return ErrSeedInvalidFingerprint
	}
	if s.SerialNumber == "" {
		return fmt.Errorf("soulseed: serial_number is empty")
	}
	if s.ExpiresAt.IsZero() {
		return fmt.Errorf("soulseed: expires_at is zero")
	}
	if s.Status == "" {
		s.Status = StatusActive
	}
	if !validStatus(s.Status) {
		return fmt.Errorf("soulseed: invalid status %q", s.Status)
	}

	var issuedAtArg any
	if !s.IssuedAt.IsZero() {
		issuedAtArg = s.IssuedAt.UTC()
	}
	var issuedByKIDArg any
	if s.IssuedByKID != nil {
		issuedByKIDArg = *s.IssuedByKID
	}

	row := db.QueryRow(ctx, insertSQL,
		s.SID,
		s.Fingerprint,
		s.SerialNumber,
		issuedAtArg,
		s.ExpiresAt.UTC(),
		issuedByKIDArg,
		string(s.Status),
	)
	if err := row.Scan(&s.SeedID, &s.IssuedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			switch pgErr.ConstraintName {
			case "soul_seeds_active_by_sid_idx":
				return fmt.Errorf("%w (constraint %s): %w",
					ErrSeedActiveExists, pgErr.ConstraintName, err)
			case "soul_seeds_fingerprint_idx", "soul_seeds_serial_number_idx":
				return fmt.Errorf("%w (constraint %s): %w",
					ErrSeedFingerprintCollision, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("soulseed: unique violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "soul_seeds_sid_fk" {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrSeedSoulNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("soulseed: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("soulseed: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("soulseed: insert: %w", err)
}

// SelectActiveBySID returns the current active seed for SID, or
// [ErrSeedNotFound] if no active seed exists (new Soul without bootstrap or
// revoked Soul).
func SelectActiveBySID(ctx context.Context, db ExecQueryRower, sid string) (*SoulSeed, error) {
	row := db.QueryRow(ctx, selectActiveBySIDSQL, sid)
	s, err := scanSoulSeed(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSeedNotFound
		}
		return nil, err
	}
	return s, nil
}

// SelectByFingerprint is lookup during mTLS handshake for CRL status checks.
// It returns [ErrSeedNotFound] if fingerprint is unknown to registry.
func SelectByFingerprint(ctx context.Context, db ExecQueryRower, fingerprint string) (*SoulSeed, error) {
	if !ValidFingerprintFormat(fingerprint) {
		return nil, ErrSeedInvalidFingerprint
	}
	row := db.QueryRow(ctx, selectByFingerprintSQL, fingerprint)
	s, err := scanSoulSeed(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSeedNotFound
		}
		return nil, err
	}
	return s, nil
}

func scanSoulSeed(row pgx.Row) (*SoulSeed, error) {
	var (
		s                SoulSeed
		statusStr        string
		issuedByKID      *string
		revocationReason *string
	)
	err := row.Scan(
		&s.SeedID,
		&s.SID,
		&s.Fingerprint,
		&s.SerialNumber,
		&s.IssuedAt,
		&s.ExpiresAt,
		&issuedByKID,
		&statusStr,
		&revocationReason,
	)
	if err != nil {
		// pgx.ErrNoRows is propagated to caller; callers have more context
		// (active-select vs by-fingerprint) to map it to the right sentinel.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("soulseed: scan: %w", err)
	}
	s.Status = Status(statusStr)
	s.IssuedByKID = issuedByKID
	s.RevocationReason = revocationReason
	return &s, nil
}

// SupersedeBySID moves the existing active seed for this SID to `superseded`.
// It is a no-op (rows-affected = 0) when no active seed exists, which is valid
// during first Soul onboarding.
//
// It must be called inside the same transaction as [Insert] of the new active
// seed, otherwise partial unique is violated between supersede and insert.
func SupersedeBySID(ctx context.Context, db ExecQueryRower, sid string) error {
	if sid == "" {
		return fmt.Errorf("soulseed: sid is empty")
	}
	if _, err := db.Exec(ctx, supersedeBySIDSQL, sid); err != nil {
		return fmt.Errorf("soulseed: supersede: %w", err)
	}
	return nil
}

// orphanActiveBySIDSQL cascade-moves active seed to `orphaned` (ADR-017).
// WHERE status='active' is intentional: revoked > orphaned, so if seed is
// already revoked it must not be overwritten (security precedence).
// superseded/expired are not moved to `orphaned` either; they are already
// historical, and Reaper will pick them up by normal rules.
const orphanActiveBySIDSQL = `
UPDATE soul_seeds
SET status = 'orphaned'
WHERE sid = $1 AND status = 'active'
`

// OrphanActiveBySID moves the active seed for this SID to `orphaned`.
// It is used by keeper-side core module `core.cloud.provisioned destroyed`
// (ADR-017 cascade) inside a shared PG transaction together with
// `soul.UpdateStatus(destroyed)` and `bootstraptoken.BurnAllForSID`.
//
// No-op (rows-affected = 0) if no active seed exists (push host / already
// revoked / never onboarded), which is valid.
func OrphanActiveBySID(ctx context.Context, db ExecQueryRower, sid string) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("soulseed: sid is empty")
	}
	tag, err := db.Exec(ctx, orphanActiveBySIDSQL, sid)
	if err != nil {
		return 0, fmt.Errorf("soulseed: orphan active by sid: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Revoke marks a specific seed as revoked while preserving reason. It affects
// both active and superseded (revoking not-yet-expired superseded is useful
// during a security incident). It does not touch already expired/revoked rows.
//
// It returns affected row count (0 = seed_id does not exist or is already
// expired/revoked); caller can differentiate through SelectByFingerprint if
// important.
func Revoke(ctx context.Context, db ExecQueryRower, seedID, reason string) (int64, error) {
	if seedID == "" {
		return 0, fmt.Errorf("soulseed: seed_id is empty")
	}
	tag, err := db.Exec(ctx, revokeSQL, seedID, reason)
	if err != nil {
		return 0, fmt.Errorf("soulseed: revoke: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListFilter is filter for [SelectAll].
type ListFilter struct {
	SID    string
	Status Status
}

// SelectAll returns seed history with the applied filter. Sorting is
// `issued_at DESC, seed_id ASC`.
func SelectAll(ctx context.Context, db ExecQueryRower, filter ListFilter, offset, limit int) ([]*SoulSeed, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("soulseed: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("soulseed: limit must be >= 1, got %d", limit)
	}
	whereSQL, args := buildListWhere(filter)

	countSQL := "SELECT COUNT(*) FROM soul_seeds" + whereSQL
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("soulseed: count: %w", err)
	}

	listSQL := `SELECT seed_id, sid, fingerprint, serial_number,
       issued_at, expires_at, issued_by_kid, status, revocation_reason
FROM soul_seeds` + whereSQL +
		fmt.Sprintf(" ORDER BY issued_at DESC, seed_id ASC OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("soulseed: list query: %w", err)
	}
	defer rows.Close()

	var out []*SoulSeed
	for rows.Next() {
		s, err := scanSoulSeed(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("soulseed: list iter: %w", err)
	}
	return out, total, nil
}

func buildListWhere(f ListFilter) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	if f.SID != "" {
		args = append(args, f.SID)
		clauses = append(clauses, fmt.Sprintf("sid = $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(clauses) == 0 {
		return "", nil
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	return where, args
}
