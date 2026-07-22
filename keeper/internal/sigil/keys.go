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

// File keys.go — CRUD of sigil_signing_keys registry (migration 037, ADR-026(h),
// R3 multi-anchor). Separate from store.go: store.go manages plugin_sigils
// (allow/deny stamps for specific binaries), keys.go — trust-anchor keys for SIGNING
// those stamps. Entities are independent, shared package only because both are about
// Sigil.
//
// Security invariant: PRIVATE KEY NEVER in Postgres. SigningKey holds
// only the public part (PubkeyPEM) and reference to the private key in Vault (VaultRef).
//
// Two integrity invariants (held transactionally, pattern — operator.Revoke
// with FOR UPDATE):
//   - ≥1 active: cannot [Retire] the last active key ([ErrLastActiveKey]) —
//     otherwise Sigil verification loses all anchors (symmetry with self-lockout RBAC);
//   - exactly one primary among active: [Introduce] with makePrimary, [SetPrimary]
//     clear the old primary and set new in ONE transaction; partial unique
//     index sigil_signing_keys_one_primary — last defense against races.

// SigningKey — row of sigil_signing_keys registry (migration 037).
//
// PubkeyPEM — PUBLIC part (SPKI PEM), sent to Soul as trust-anchor. VaultRef
// — where the private key lives in Vault KV. Private key is not and cannot be
// in this struct (security invariant ADR-026(d)).
type SigningKey struct {
	ID              int64
	KeyID           string // stable id: SHA-256 of SPKI-DER, hex
	PubkeyPEM       string // PUBLIC part only (SPKI PEM)
	VaultRef        string // reference to private key in Vault KV
	IsPrimary       bool
	Status          string // active | retired
	IntroducedAt    time.Time
	IntroducedByAID *string
	RetiredAt       *time.Time
	RetiredByAID    *string
}

// Sentinel errors for key CRUD.
var (
	// ErrKeyNotFound — key with this key_id not found (or not active where
	// operation requires active).
	ErrKeyNotFound = errors.New("sigil: signing key not found")

	// ErrKeyAlreadyExists — Introduce with already existing key_id (UNIQUE).
	ErrKeyAlreadyExists = errors.New("sigil: signing key with this key_id already exists")

	// ErrLastActiveKey — Retire of the last active key is forbidden: the set of
	// trust-anchors must not be empty (otherwise Sigil verification loses all anchors).
	// Symmetry with self-lockout RBAC.
	ErrLastActiveKey = errors.New("sigil: cannot retire the last active signing key")

	// ErrRetirePrimary — Retire of primary key is forbidden directly: must first
	// transfer primary to another active key via [SetPrimary]. This ensures primary
	// never "disappears", and the invariant "exactly one primary among active"
	// holds without an intermediate state "active keys exist but no primary".
	ErrRetirePrimary = errors.New("sigil: cannot retire the primary key; SetPrimary to another active key first")

	// ErrKeyRetired — operation requires an active key (SetPrimary), but the target
	// is retired.
	ErrKeyRetired = errors.New("sigil: signing key is retired")

	// ErrConcurrentPrimary — race in setting primary: partial unique index
	// sigil_signing_keys_one_primary gave 23505 (two concurrent transactions
	// simultaneously set primary on different keys — clearActivePrimary of one
	// did not see the insert/update of the other before commit). This is NOT ErrKeyAlreadyExists
	// (key_id conflict): the key itself is valid, only the "exactly one primary" invariant
	// conflicts. API maps to 409 (retry-able: subsequent Introduce/SetPrimary
	// will already see the fixed primary).
	ErrConcurrentPrimary = errors.New("sigil: concurrent primary-key change (one_primary index conflict); retry")
)

// onePrimaryConstraint — name of the partial unique index "exactly one primary among
// active" (migration 037). [mapKeyInsertError] uses it to distinguish primary race
// ([ErrConcurrentPrimary]) from key_id conflict ([ErrKeyAlreadyExists]) — both
// give SQLSTATE 23505.
const onePrimaryConstraint = "sigil_signing_keys_one_primary"

// KeyStorePool — narrow subset of pgxpool.Pool, needed for keys CRUD: read/exec
// plus BeginTx for atomic multi-statement operations (Introduce-makePrimary
// / Retire / SetPrimary). Declared locally (like operator.ServicePool); actual
// pool and pgx.Tx satisfy automatically — tested via fake-pool.
type KeyStorePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ KeyStorePool = (*pgxpool.Pool)(nil)

const (
	insertSigningKeySQL = `
INSERT INTO sigil_signing_keys (key_id, pubkey_pem, vault_ref, is_primary, introduced_by_aid)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, introduced_at
`

	// clearActivePrimarySQL clears the primary flag from ALL active-primary rows
	// (at most one per invariant). Executed BEFORE setting new
	// primary in the same transaction — so partial unique index doesn't fire.
	clearActivePrimarySQL = `
UPDATE sigil_signing_keys
SET is_primary = false
WHERE status = 'active' AND is_primary
`

	// setPrimaryByKeyIDSQL sets primary on an active key. WHERE status='active'
	// — atomic guard: a retired key cannot become primary (rows-affected = 0).
	setPrimaryByKeyIDSQL = `
UPDATE sigil_signing_keys
SET is_primary = true
WHERE key_id = $1 AND status = 'active'
`

	// lockActiveKeysSQL locks all active rows (FOR UPDATE without aggregate —
	// PG forbids count(*) FOR UPDATE) and returns their ids for counting in Go.
	// Serializes concurrent Retire the same way as LockEffectiveClusterAdmins in
	// RBAC: acquires locks on the entire active set before checking the invariant.
	lockActiveKeysSQL = `
SELECT id FROM sigil_signing_keys WHERE status = 'active' FOR UPDATE
`

	// selectKeyByIDForUpdateSQL reads the target row under lock for
	// checking invariants in the same transaction as UPDATE.
	selectKeyByIDForUpdateSQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE key_id = $1
FOR UPDATE
`

	retireByKeyIDSQL = `
UPDATE sigil_signing_keys
SET status = 'retired', is_primary = false, retired_at = NOW(), retired_by_aid = $2
WHERE key_id = $1 AND status = 'active'
`

	listActiveKeysSQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE status = 'active'
ORDER BY is_primary DESC, introduced_at ASC, id ASC
`

	getPrimarySQL = `
SELECT id, key_id, pubkey_pem, vault_ref, is_primary, status,
       introduced_at, introduced_by_aid, retired_at, retired_by_aid
FROM sigil_signing_keys
WHERE status = 'active' AND is_primary
`

	// listAllKeyIDsSQL returns key_id for ALL rows without status filter —
	// authoritative set of "live" private keys for orphan-reconcile (retired
	// is also live: key is needed to verify old Sigils).
	listAllKeyIDsSQL = `
SELECT key_id FROM sigil_signing_keys
`
)

// Introduce adds a new trust-anchor key as active. If makePrimary —
// the old active primary is cleared and the new one becomes primary in ONE
// transaction (invariant "exactly one primary among active").
//
// Only the public part (pubkeyPEM) + reference to the private key in Vault
// (vaultRef) is stored; private key does not enter Postgres.
//
// Errors:
//   - keyID/pubkeyPEM/vaultRef empty → validation error (before query);
//   - key_id already exists → [ErrKeyAlreadyExists];
//   - introducedByAID points to non-existent operator → FK violation
//     (wrapped). NULL is allowed (bootstrap/seed without initiator).
func Introduce(ctx context.Context, pool KeyStorePool, keyID, pubkeyPEM, vaultRef string, makePrimary bool, introducedByAID *string) (*SigningKey, error) {
	if keyID == "" {
		return nil, fmt.Errorf("sigil: key_id is empty")
	}
	if pubkeyPEM == "" {
		return nil, fmt.Errorf("sigil: pubkey_pem is empty")
	}
	if vaultRef == "" {
		return nil, fmt.Errorf("sigil: vault_ref is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// makePrimary: clear old primary BEFORE inserting new primary (otherwise
	// partial unique index sigil_signing_keys_one_primary will give 23505).
	if makePrimary {
		if _, err := tx.Exec(ctx, clearActivePrimarySQL); err != nil {
			return nil, fmt.Errorf("sigil: clear active primary: %w", err)
		}
	}

	key := &SigningKey{
		KeyID:           keyID,
		PubkeyPEM:       pubkeyPEM,
		VaultRef:        vaultRef,
		IsPrimary:       makePrimary,
		Status:          "active",
		IntroducedByAID: introducedByAID,
	}
	err = tx.QueryRow(ctx, insertSigningKeySQL,
		keyID, pubkeyPEM, vaultRef, makePrimary, nullableAID(introducedByAID),
	).Scan(&key.ID, &key.IntroducedAt)
	if err != nil {
		return nil, mapKeyInsertError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sigil: commit tx: %w", err)
	}
	return key, nil
}

// SetPrimary makes an active key primary: clears the old primary and sets new
// in one transaction. The target key must be active.
//
// Errors:
//   - key_id not found → [ErrKeyNotFound];
//   - target key is retired → [ErrKeyRetired].
func SetPrimary(ctx context.Context, pool KeyStorePool, keyID, callerAID string) error {
	if keyID == "" {
		return fmt.Errorf("sigil: key_id is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, err := selectKeyForUpdate(ctx, tx, keyID)
	if err != nil {
		return err
	}
	if target.Status != "active" {
		return ErrKeyRetired
	}
	if target.IsPrimary {
		// Already primary — no-op, but valid (idempotent). Commit empty tx.
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, clearActivePrimarySQL); err != nil {
		return fmt.Errorf("sigil: clear active primary: %w", err)
	}
	if _, err := tx.Exec(ctx, setPrimaryByKeyIDSQL, keyID); err != nil {
		return mapSetPrimaryError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sigil: commit tx: %w", err)
	}
	return nil
}

// Retire removes a key from the trust-anchor set (status=retired). Two
// invariants checked in a transaction under FOR UPDATE:
//   - cannot retire the last active key → [ErrLastActiveKey];
//   - cannot retire primary directly → [ErrRetirePrimary] (first SetPrimary
//     to another active key).
//
// Errors:
//   - callerAID empty → error (audit invariant: who retired it is required);
//   - key_id not found → [ErrKeyNotFound];
//   - key already retired → [ErrKeyNotFound] (no active record for this key).
func Retire(ctx context.Context, pool KeyStorePool, keyID, callerAID string) error {
	if keyID == "" {
		return fmt.Errorf("sigil: key_id is empty")
	}
	if callerAID == "" {
		return fmt.Errorf("sigil: retired_by_aid is empty")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("sigil: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize concurrent Retire: lock entire active set under FOR
	// UPDATE (pattern — rbac.LockEffectiveClusterAdmins). Lock order
	// is deterministic: first all active, then the target row.
	activeCount, err := countLockedActive(ctx, tx)
	if err != nil {
		return fmt.Errorf("sigil: lock active keys: %w", err)
	}

	target, err := selectKeyForUpdate(ctx, tx, keyID)
	if err != nil {
		return err
	}
	if target.Status != "active" {
		// retired key — no active record for this key.
		return ErrKeyNotFound
	}
	if target.IsPrimary {
		return ErrRetirePrimary
	}
	// activeCount includes target; forbidden if it's the only active.
	if activeCount <= 1 {
		return ErrLastActiveKey
	}

	tag, err := tx.Exec(ctx, retireByKeyIDSQL, keyID, callerAID)
	if err != nil {
		return fmt.Errorf("sigil: retire: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Race: between select-FOR-UPDATE and UPDATE no one could modify the row
		// (it's under lock) — defensive, should not happen.
		return ErrKeyNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sigil: commit tx: %w", err)
	}
	return nil
}

// ListActiveKeys returns all active trust-anchor keys. Order is stable:
// primary first, then by introduction time (introduced_at, id). This is the future
// set for SigilTrustAnchors (R3-S6 broadcast).
//
// Name with Keys suffix — to avoid collision with [ListActive] for plugin_sigils
// (store.go): the package hosts both registries (stamps and their signing keys).
func ListActiveKeys(ctx context.Context, db ExecQueryRower) ([]*SigningKey, error) {
	rows, err := db.Query(ctx, listActiveKeysSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list active keys: %w", err)
	}
	defer rows.Close()

	var out []*SigningKey
	for rows.Next() {
		k, err := scanSigningKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list active keys rows: %w", err)
	}
	return out, nil
}

// ListAllKeyIDs returns key_id for ALL rows of sigil_signing_keys regardless of
// status (active AND retired — retired is also live: its key is needed to verify
// old Sigils). This is the authoritative set of "live" keys for orphan-reconcile: anything
// in Vault under `secret/keeper/sigil-keys/<key_id>` but NOT in this set —
// is an orphan candidate.
//
// Return — set (map[string]struct{}) for O(1) lookup in reconcile loop.
func ListAllKeyIDs(ctx context.Context, db ExecQueryRower) (map[string]struct{}, error) {
	rows, err := db.Query(ctx, listAllKeyIDsSQL)
	if err != nil {
		return nil, fmt.Errorf("sigil: list all key ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return nil, fmt.Errorf("sigil: scan key id: %w", err)
		}
		out[keyID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sigil: list all key ids rows: %w", err)
	}
	return out, nil
}

// GetPrimaryKey returns the active primary key (the one Keeper uses to sign
// new Sigils). [ErrKeyNotFound] if no primary exists (set is empty).
func GetPrimaryKey(ctx context.Context, db ExecQueryRower) (*SigningKey, error) {
	row := db.QueryRow(ctx, getPrimarySQL)
	k, err := scanSigningKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return k, nil
}

// countLockedActive locks all active rows (FOR UPDATE) and returns their
// count. We count in Go because PG forbids count(*) with FOR UPDATE.
func countLockedActive(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, lockActiveKeysSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// selectKeyForUpdate reads a row under FOR UPDATE; pgx.ErrNoRows → ErrKeyNotFound.
func selectKeyForUpdate(ctx context.Context, tx pgx.Tx, keyID string) (*SigningKey, error) {
	row := tx.QueryRow(ctx, selectKeyByIDForUpdateSQL, keyID)
	k, err := scanSigningKey(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return k, nil
}

// scanSigningKey — common Scan for sigil_signing_keys row.
func scanSigningKey(row pgx.Row) (*SigningKey, error) {
	var k SigningKey
	err := row.Scan(
		&k.ID,
		&k.KeyID,
		&k.PubkeyPEM,
		&k.VaultRef,
		&k.IsPrimary,
		&k.Status,
		&k.IntroducedAt,
		&k.IntroducedByAID,
		&k.RetiredAt,
		&k.RetiredByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("sigil: scan signing key: %w", err)
	}
	return &k, nil
}

// mapKeyInsertError maps pgx errors from Introduce to sentinels.
func mapKeyInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// UNIQUE(key_id) or partial one_primary — both SQLSTATE 23505. By
			// constraint name we distinguish: one_primary — concurrent race
			// in setting primary ([ErrConcurrentPrimary], retry-able 409); otherwise
			// key_id conflict ([ErrKeyAlreadyExists]).
			if pgErr.ConstraintName == onePrimaryConstraint {
				return fmt.Errorf("%w (constraint %s): %w", ErrConcurrentPrimary, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("%w (constraint %s): %w", ErrKeyAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("sigil: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("sigil: insert signing key: %w", err)
}

// mapSetPrimaryError maps pgx errors from UPDATE of primary to sentinels. Race
// on one_primary index (23505) → [ErrConcurrentPrimary] (like in [mapKeyInsertError]);
// otherwise — wrapped err.
func mapSetPrimaryError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeUniqueViolation &&
		pgErr.ConstraintName == onePrimaryConstraint {
		return fmt.Errorf("%w (constraint %s): %w", ErrConcurrentPrimary, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("sigil: set primary: %w", err)
}

// nullableAID converts *string to any for pgx argument (nil → SQL NULL).
func nullableAID(aid *string) any {
	if aid == nil {
		return nil
	}
	return *aid
}
