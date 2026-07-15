package bootstraptoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors for the CRUD layer. The handler side maps them:
//
//   - ErrTokenActiveExists  → 409 conflict (SID already has an active
//     token, per the partial unique `bootstrap_tokens_active_by_sid_idx`).
//     The operator must revoke the old one or wait for TTL.
//   - ErrTokenInvalid       → 403 forbidden — token doesn't exist, expired,
//     or already burned. Returned from [Burn]; deliberately doesn't
//     distinguish the reason (protects against user-enum attacks — all
//     three cases return the same error).
//   - ErrTokenSoulNotFound  → 404 on Insert when the SID isn't in souls.
var (
	ErrTokenActiveExists = errors.New("bootstraptoken: active token for SID already exists")
	ErrTokenInvalid      = errors.New("bootstraptoken: token invalid (not found, expired, or already used)")
	ErrTokenSoulNotFound = errors.New("bootstraptoken: target SID not found in souls registry")
	ErrTokenNotFound     = errors.New("bootstraptoken: token_id not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower is the narrow subset of the pgxpool.Pool interface the
// CRUD layer needs. Symmetric with [operator.ExecQueryRower] /
// [soul.ExecQueryRower].
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
INSERT INTO bootstrap_tokens (sid, token_hash, expires_at, created_by_aid)
VALUES ($1, $2, $3, $4)
RETURNING token_id, created_at
`

const selectByHashSQL = `
SELECT token_id, sid, token_hash, created_at, expires_at,
       used_at, used_by_kid, created_by_aid
FROM bootstrap_tokens
WHERE token_hash = $1
`

// burnSQL is the race-safe UPDATE that "burns" a token. The WHERE
// conjunction guarantees that a concurrent double presentation yields one
// UPDATE and one miss. RETURNING token_id is for audit and to check
// rows-affected in a single round trip.
//
// Parameters:
//
//	$1 — token_hash of the presented plain token.
//	$2 — sid from BootstrapRequest (guards against SID substitution under
//	     the same hash).
//	$3 — kid of the Keeper instance handling the request.
const burnSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $3
WHERE token_hash = $1
  AND sid        = $2
  AND used_at    IS NULL
  AND expires_at > NOW()
RETURNING token_id
`

// Insert writes a new bootstrap token (operator issuance). Returns the
// created [Record] with TokenID and CreatedAt populated.
//
// Pre-conditions:
//   - sid — a valid SID in the souls registry (FK checked by PG);
//   - tokenHash — SHA-256 hex (64 lower-hex);
//   - ttl > 0 (expires_at = NOW() + ttl).
//
// Returns:
//   - [ErrTokenActiveExists] on UNIQUE violation of the `_active_by_sid`
//     partial index.
//   - [ErrTokenSoulNotFound] on FK violation of `bootstrap_tokens_sid_fk`.
//   - a wrapped fmt.Errorf for other pg errors.
func Insert(ctx context.Context, db ExecQueryRower, sid, tokenHash string, ttl time.Duration, createdByAID *string) (*Record, error) {
	if sid == "" {
		return nil, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if !ValidHashFormat(tokenHash) {
		return nil, errInvalidHash
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("bootstraptoken: ttl must be positive, got %s", ttl)
	}

	expiresAt := time.Now().UTC().Add(ttl)
	var createdByAIDArg any
	if createdByAID != nil {
		createdByAIDArg = *createdByAID
	}

	rec := &Record{
		SID:          sid,
		TokenHash:    tokenHash,
		ExpiresAt:    expiresAt,
		CreatedByAID: createdByAID,
	}
	row := db.QueryRow(ctx, insertSQL, sid, tokenHash, expiresAt, createdByAIDArg)
	if err := row.Scan(&rec.TokenID, &rec.CreatedAt); err != nil {
		return nil, mapInsertError(err)
	}
	return rec, nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// partial unique index `_active_by_sid` or `_token_hash`.
			// Differentiate by constraint name for the UX handler:
			// hash collision (de facto impossible) vs "active token
			// already issued for this SID".
			if pgErr.ConstraintName == "bootstrap_tokens_token_hash_idx" {
				return fmt.Errorf("bootstraptoken: token_hash collision (constraint %s): %w",
					pgErr.ConstraintName, err)
			}
			return fmt.Errorf("%w (constraint %s): %w",
				ErrTokenActiveExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "bootstrap_tokens_sid_fk" {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrTokenSoulNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("bootstraptoken: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("bootstraptoken: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("bootstraptoken: insert: %w", err)
}

// SelectByHash reads a Record by token_hash. Returns [ErrTokenNotFound] on
// pgx.ErrNoRows.
//
// Used by the gRPC Bootstrap handler before Burn — to get the SID and
// payload for the audit event, **if** the caller presented a valid SID.
// Burn itself remains atomic (via the WHERE clause).
func SelectByHash(ctx context.Context, db ExecQueryRower, tokenHash string) (*Record, error) {
	if !ValidHashFormat(tokenHash) {
		return nil, errInvalidHash
	}
	row := db.QueryRow(ctx, selectByHashSQL, tokenHash)
	return scanRecord(row)
}

func scanRecord(row pgx.Row) (*Record, error) {
	var (
		rec          Record
		usedAt       *time.Time
		usedByKID    *string
		createdByAID *string
	)
	err := row.Scan(
		&rec.TokenID,
		&rec.SID,
		&rec.TokenHash,
		&rec.CreatedAt,
		&rec.ExpiresAt,
		&usedAt,
		&usedByKID,
		&createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("bootstraptoken: scan: %w", err)
	}
	rec.UsedAt = usedAt
	rec.UsedByKID = usedByKID
	rec.CreatedByAID = createdByAID
	return &rec, nil
}

// Burn race-safely burns a token when a Soul presents it to the
// `Bootstrap` RPC. MUST run inside the same transaction as
// `soul.UpdateStatus` (pending → connected) and `soulseed.Insert` — the
// caller (gRPC handler) is responsible for that invariant.
//
// Parameters:
//   - tokenHash — SHA-256 hex of the plain token presented by the client.
//   - claimedSID — SID from BootstrapRequest.sid (guards against SID
//     substitution under the same hash — an attacker can't "hijack" a
//     token onto someone else's SID).
//   - usedByKID — KID of the Keeper instance handling the request.
//
// Returns:
//   - tokenID — UUID of the burned record (for the audit payload).
//   - [ErrTokenInvalid] — token doesn't exist, expired, already used, or
//     the SID doesn't match. Deliberately doesn't distinguish the reason
//     (anti-enum).
//   - a wrapped fmt.Errorf for other pg errors.
func Burn(ctx context.Context, db ExecQueryRower, tokenHash, claimedSID, usedByKID string) (string, error) {
	if !ValidHashFormat(tokenHash) {
		return "", errInvalidHash
	}
	if claimedSID == "" {
		return "", fmt.Errorf("bootstraptoken: claimedSID is empty")
	}
	if usedByKID == "" {
		return "", fmt.Errorf("bootstraptoken: usedByKID is empty")
	}

	var tokenID string
	err := db.QueryRow(ctx, burnSQL, tokenHash, claimedSID, usedByKID).Scan(&tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTokenInvalid
		}
		return "", fmt.Errorf("bootstraptoken: burn: %w", err)
	}
	return tokenID, nil
}

// SystemKIDCloudDestroy is the special `used_by_kid` value for records
// "burned" by the `core.cloud.provisioned destroyed` cascade handler
// (ADR-017): the host was deleted along with the VM, no real token
// presentation happened, and no Archon operator was involved either, so
// we record a system marker instead. The format differs from a valid
// `kid` (kebab-case `keeper-XXX`): `system-cloud-destroy` can't collide
// with a real KID, and the audit handler filters on this prefix.
const SystemKIDCloudDestroy = "system-cloud-destroy"

// burnAllForSIDSQL cascade-burns every not-yet-used token for a given SID.
// Unlike [Burn], it does NOT check expires_at > NOW(): at cloud-destroy
// time any still-active token is moot (the host no longer exists), even
// if it expired a second ago — the Reaper will delete the record via
// purge_used_tokens anyway. What matters here is recording the fact
// "burned at cloud-destroy time", so the anti-replay invariant holds
// without racing the TTL boundary.
const burnAllForSIDSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $2
WHERE sid     = $1
  AND used_at IS NULL
`

// BurnAllForSID cascade-burns every not-yet-used bootstrap token for a
// given SID. Used by the keeper-side `core.cloud.provisioned destroyed`
// core module (ADR-017 cascade) within the same PG transaction as
// `soul.UpdateStatus(destroyed)` and the `soulseed` cascade update.
//
// `usedByKID` is a KID or a special marker (see [SystemKIDCloudDestroy]).
//
// Returns the number of affected rows (0 = the SID had no active tokens,
// the normal case for a long-running Soul).
func BurnAllForSID(ctx context.Context, db ExecQueryRower, sid, usedByKID string) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if usedByKID == "" {
		return 0, fmt.Errorf("bootstraptoken: usedByKID is empty")
	}
	tag, err := db.Exec(ctx, burnAllForSIDSQL, sid, usedByKID)
	if err != nil {
		return 0, fmt.Errorf("bootstraptoken: burn all for sid: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SystemKIDForceReissue is the special `used_by_kid` value for a token
// invalidated by the operator via `issue-token?force=true` (a new token
// is issued in place of a still-active one). No real presentation by a
// Soul happened — we record a system marker so audit can distinguish
// force-reissue from a genuine Burn in the Bootstrap RPC. The format
// differs from a valid KID (`keeper-XXX`) — collisions are excluded.
const SystemKIDForceReissue = "system-force-reissue"

// expireActiveBySIDSQL invalidates a SID's still-active token on
// force-reissue. Sets `used_at = NOW()`, which both (1) makes the token
// ineligible for Burn (the WHERE `used_at IS NULL` no longer matches) and
// (2) frees the partial-unique slot `bootstrap_tokens_active_by_sid_idx`
// (`WHERE used_at IS NULL`) for the subsequent Insert of a new token.
//
// Just setting `expires_at = NOW()` would NOT work: the record would
// still have `used_at IS NULL` and keep holding the unique slot.
const expireActiveBySIDSQL = `
UPDATE bootstrap_tokens
SET used_at     = NOW(),
    used_by_kid = $2
WHERE sid     = $1
  AND used_at IS NULL
RETURNING token_id
`

// ExpireActiveBySID invalidates a SID's active (not-yet-used) token and
// returns its token_id. Used by the Operator API for
// `issue-token?force=true` within the same transaction as the Insert of
// the new token (the caller is responsible for the atomicity invariant).
//
// `usedByKID` is a special marker (see [SystemKIDForceReissue]).
//
// Returns (tokenID, true, nil) if an active token existed and was
// invalidated; ("", false, nil) if there was no active token (force on a
// clean SID is normal — just an Insert); an error on pg failure.
func ExpireActiveBySID(ctx context.Context, db ExecQueryRower, sid, usedByKID string) (string, bool, error) {
	if sid == "" {
		return "", false, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if usedByKID == "" {
		return "", false, fmt.Errorf("bootstraptoken: usedByKID is empty")
	}
	var tokenID string
	err := db.QueryRow(ctx, expireActiveBySIDSQL, sid, usedByKID).Scan(&tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("bootstraptoken: expire active for sid: %w", err)
	}
	return tokenID, true, nil
}

// DeleteByTokenID deletes a record by PK. Used by the Reaper for the
// `purge_used_tokens` rule (see ADR-022 / docs/keeper/reaper.md).
//
// Returns [ErrTokenNotFound] if no record with that token_id exists.
func DeleteByTokenID(ctx context.Context, db ExecQueryRower, tokenID string) error {
	tag, err := db.Exec(ctx, `DELETE FROM bootstrap_tokens WHERE token_id = $1`, tokenID)
	if err != nil {
		return fmt.Errorf("bootstraptoken: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return nil
}
