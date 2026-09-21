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
// `Bootstrap` RPC for the FIRST time. MUST run inside the same transaction as
// the seed write — the caller (gRPC handler) is responsible for that invariant.
//
// A later presentation of the same token is NOT this function's business: it is
// refused here by `used_at IS NULL`, and admitted — under much narrower
// conditions — by [RedeemAgain]. See its godoc for why the burn alone is not a
// sufficient anti-replay primitive.
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

// redeemAgainSQL re-authorizes a token that is ALREADY burned, so a host whose
// Bootstrap reply was lost in flight can complete the onboarding the burn
// already committed to (NIM-865, docs/adr/0090-bootstrap-reply-loss-recovery.md).
//
// The whole authorization is this one statement, deliberately. It reaches
// across three tables because the conjunction below is what makes a
// re-presentation safe, and evaluating any part of it in Go first reintroduces
// the read-then-write window this fix exists to close. The same argument, in
// the same words, is why [soul.reuseForProvisionSQL] carries its predicate in
// the statement.
//
// Parameters:
//
//	$1 — token_hash of the presented plain token.
//	$2 — sid from BootstrapRequest.
//	$3 — kid of the Keeper instance handling the request.
//	$4 — [SystemKIDs]: the markers that are NOT a Soul's presentation.
//	$5 — SHA-256 of the presented CSR's SubjectPublicKeyInfo.
//
// The five conditions, and what each one is holding shut:
//
//   - `used_at IS NOT NULL` — this path is ONLY for an already-burned token. A
//     first presentation belongs to [Burn] and must not reach here.
//   - `expires_at > NOW()` — recovery never outlives the token's own TTL.
//   - used_by_kid, coalesced to the empty string, differs from every marker in
//     $4 — a token killed by an operator (force-reissue) or by a cascade was
//     never redeemed by anyone, and must not be resurrected. The coalesce is
//     what keeps the condition ABOVE from being dead weight: NULL <> ALL(…) is
//     NULL, so a bare comparison would refuse an unburned token here and the
//     used_at test could be deleted with every guard still green. Each clause
//     now fails on its own removal, which is the only way a conjunction stays
//     honest.
//   - `souls.last_seen_at IS NULL` — the host has never held a stream. This is
//     what keeps the window from becoming a hole: once the Soul has appeared,
//     the credential demonstrably arrived and the token is finished, so a leaked
//     plaintext cannot be turned against a working host.
//   - the active seed's fingerprint equals $5 — the one-shot binding. The
//     fingerprint is the SubjectPublicKeyInfo hash ([soulseed.FingerprintFromCert]),
//     so this admits exactly the keypair the first presentation bound and refuses
//     an attacker's own CSR, which is the whole of what anti-replay defends.
const redeemAgainSQL = `
UPDATE bootstrap_tokens t
SET used_at     = NOW(),
    used_by_kid = $3
WHERE t.token_hash  = $1
  AND t.sid         = $2
  AND t.used_at     IS NOT NULL
  AND t.expires_at  > NOW()
  AND coalesce(t.used_by_kid, '') <> ALL($4::text[])
  AND EXISTS (
      SELECT 1 FROM souls s
       WHERE s.sid = t.sid
         AND s.last_seen_at IS NULL
  )
  AND EXISTS (
      SELECT 1 FROM soul_seeds sd
       WHERE sd.sid         = t.sid
         AND sd.status      = 'active'
         AND sd.fingerprint = $5
  )
RETURNING t.token_id
`

// RedeemAgain re-authorizes an already-burned token for the host that burned
// it, when that host never received the reply. MUST run inside the same
// transaction as the seed refresh — the caller (gRPC handler) owns that
// invariant, symmetrically with [Burn].
//
// csrFingerprint is the SHA-256 of the presented CSR's SubjectPublicKeyInfo
// (see [soulseed.FingerprintFromCSR]); it is compared against the seed the
// first presentation issued, which is bound to the same value because
// re-signing one key does not move its fingerprint.
//
// Returns [ErrTokenInvalid] when the statement matches nothing — for every
// reason, undistinguished, exactly as [Burn] does. The caller maps that to the
// same single PermissionDenied, so a probe cannot learn from this path what it
// could not learn from a first presentation.
func RedeemAgain(ctx context.Context, db ExecQueryRower, tokenHash, claimedSID, usedByKID, csrFingerprint string) (string, error) {
	if !ValidHashFormat(tokenHash) {
		return "", errInvalidHash
	}
	if claimedSID == "" {
		return "", fmt.Errorf("bootstraptoken: claimedSID is empty")
	}
	if usedByKID == "" {
		return "", fmt.Errorf("bootstraptoken: usedByKID is empty")
	}
	// A real KID reaching this argument would let a Keeper whose instance id
	// happened to match a marker redeem what the marker forbids.
	if isSystemKID(usedByKID) {
		return "", fmt.Errorf("bootstraptoken: usedByKID %q is a system marker", usedByKID)
	}
	if !ValidHashFormat(csrFingerprint) {
		return "", fmt.Errorf("bootstraptoken: csrFingerprint format invalid (must be 64 lower-hex chars)")
	}

	var tokenID string
	err := db.QueryRow(ctx, redeemAgainSQL, tokenHash, claimedSID, usedByKID, SystemKIDs(), csrFingerprint).Scan(&tokenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTokenInvalid
		}
		return "", fmt.Errorf("bootstraptoken: redeem again: %w", err)
	}
	return tokenID, nil
}

// closeRecoveryBySIDSQL re-marks a SID's already-BURNED but still-unexpired
// tokens, so [RedeemAgain] refuses them (NIM-865).
//
// Without it an operator has no way to shut a recovery window that is already
// open. Every existing "kill this token" statement — [expireActiveBySIDSQL],
// [burnAllForSIDSQL] — matches `used_at IS NULL`, because before the recovery
// path a burned token was inert by definition. It no longer is: it stays
// redeemable by the key it bound until the host connects or the TTL runs out.
// So `issue-token --force` on a host whose token leaked AFTER being burned
// would have reported success and changed nothing that mattered.
//
// Only `used_by_kid` moves, to the marker, which is exactly the condition
// RedeemAgain refuses on. `used_at` is left alone here because this statement
// records no redemption — nobody presented anything, an operator revoked.
// [redeemAgainSQL] does move it, and correctly: a recovery IS a redemption, so
// the column keeps meaning "when this token was last redeemed" on both paths.
// The cost of that is real and accepted in the ADR: each recovery restarts
// `purge_used_tokens`' 90-day clock, so a replayer can keep the row alive.
const closeRecoveryBySIDSQL = `
UPDATE bootstrap_tokens
SET used_by_kid = $2
WHERE sid        = $1
  AND used_at    IS NOT NULL
  AND expires_at > NOW()
  AND coalesce(used_by_kid, '') <> ALL($3::text[])
`

// CloseRecoveryBySID shuts the reply-loss recovery window on every burned,
// unexpired token of a SID, and reports how many it closed.
//
// Call it wherever an operator's intent is "this host's outstanding tokens
// must stop working" — the same places that already call [ExpireActiveBySID].
// Those two are complements, not alternatives: that one frees the active-token
// slot, this one disarms the burned ones it cannot see.
func CloseRecoveryBySID(ctx context.Context, db ExecQueryRower, sid, marker string) (int64, error) {
	if sid == "" {
		return 0, fmt.Errorf("bootstraptoken: sid is empty")
	}
	if !isSystemKID(marker) {
		return 0, fmt.Errorf("bootstraptoken: marker %q is not a system marker", marker)
	}
	tag, err := db.Exec(ctx, closeRecoveryBySIDSQL, sid, marker, SystemKIDs())
	if err != nil {
		return 0, fmt.Errorf("bootstraptoken: close recovery for sid: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SystemKIDs returns every `used_by_kid` marker that records an invalidation
// rather than a Soul's presentation. A token carrying one of these was never
// redeemed by a host, so there is no lost reply to recover and
// [RedeemAgain] must refuse it.
//
// Returned as a slice rather than checked with a `system-%` LIKE: a marker is
// free to be named anything, and a prefix test would silently admit the first
// one that isn't. Drift is caught by TestSystemKIDs_CoversEveryMarker, which
// fails when a new `SystemKID*` constant is declared without being listed here.
func SystemKIDs() []string {
	return []string{
		SystemKIDCloudDestroy,
		SystemKIDForceReissue,
		SystemKIDCloudReprovision,
		SystemKIDBootstrapIssuedReissue,
		SystemKIDSoulForget,
	}
}

func isSystemKID(kid string) bool {
	for _, m := range SystemKIDs() {
		if kid == m {
			return true
		}
	}
	return false
}

// SystemKIDCloudDestroy is the special `used_by_kid` value for records
// "burned" by the teardown cascade handler (the removed `core.cloud.provisioned destroyed`)
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
// given SID. Used by the teardown cascade (the removed `core.cloud.provisioned destroyed`)
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

// SystemKIDCloudReprovision is the special `used_by_kid` value for a token
// invalidated because its SID is being provisioned again (NIM-170): the VM the
// token was baked for never onboarded, and the replacement VM gets a fresh
// token. Same mechanism as force-reissue, distinct marker so audit can tell a
// re-provision from an operator's manual reissue.
const SystemKIDCloudReprovision = "system-cloud-reprovision"

// SystemKIDBootstrapIssuedReissue marks an unused token invalidated by a
// repeated `core.bootstrap.issued` step. The ready-made VM has not onboarded;
// the previous plaintext may have been lost in an interrupted delivery, so the
// whole batch receives fresh one-time tokens. A distinct marker keeps this
// scenario retry separate from cloud reprovision and manual API force-reissue.
const SystemKIDBootstrapIssuedReissue = "system-bootstrap-issued-reissue"

// SystemKIDSoulForget is the special `used_by_kid` value for a token burned
// because its host was forgotten by an operator (`soul.forget`, NIM-386).
// Distinct from [SystemKIDCloudDestroy]: the cloud cascade means "the VM is
// gone", this one means "the Keeper is done with this host" and says nothing
// about whether the machine is still running. Same non-KID format, so it
// cannot collide with a real `keeper-XXX`.
const SystemKIDSoulForget = "system-soul-forget"

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
