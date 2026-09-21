package cert

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors of the CRUD layer.
var (
	// ErrActiveExists — attempt to insert a second active material for the
	// same (incarnation, kind) (partial unique
	// `warrant_active_by_incarnation_kind_idx`). Caller must first Supersede
	// the old one, then Insert the new — in one tx.
	ErrActiveExists = errors.New("cert: active warrant for (incarnation, kind) already exists (call SupersedeActive first)")

	// ErrFingerprintCollision — fingerprint already exists in the registry.
	// De facto impossible (SHA-256 of a public key is unique); constraint
	// kept explicit.
	ErrFingerprintCollision = errors.New("cert: fingerprint already exists in registry")

	// ErrIncarnationNotFound — INSERT references a nonexistent incarnation
	// (FK `warrant_incarnation_fk`).
	ErrIncarnationNotFound = errors.New("cert: target incarnation not found")

	// ErrNotFound — Select found no row.
	ErrNotFound = errors.New("cert: warrant not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — narrow subset of pgxpool.Pool (symmetric to
// soulseed.ExecQueryRower): unit tests via fake without PG, production —
// pool/Conn/Tx.
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

// TxBeginner — narrow subset of pgxpool.Pool for transactional operations
// (supersede old active + insert new in one tx). Symmetric to
// choir.TxBeginner / incarnation.TxBeginner.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ TxBeginner = (*pgxpool.Pool)(nil)

const insertSQL = `
INSERT INTO warrant (
    incarnation_id, kind, vault_ref, serial_number, fingerprint,
    not_after, issued_at, pki_mount, pki_role, status,
    issued_by_kid, last_rotation_voyage_id, auto_rotate, rotate_threshold_override
) VALUES ($1, $2, $3, $4, $5,
    $6, COALESCE($7, NOW()), $8, $9, $10,
    $11, $12, $13, $14)
RETURNING cert_id, issued_at
`

const selectColumns = `cert_id, incarnation_id, kind, vault_ref, serial_number, fingerprint,
       not_after, issued_at, pki_mount, pki_role, status,
       issued_by_kid, last_rotation_voyage_id, auto_rotate, rotate_threshold_override`

const selectActiveSQL = `
SELECT ` + selectColumns + `
FROM warrant
WHERE incarnation_id = $1 AND kind = $2 AND status = 'active'
`

// supersedeActiveSQL moves the active row for a given (incarnation, kind) to
// superseded. Under the normal invariant affects exactly one row (or zero).
const supersedeActiveSQL = `
UPDATE warrant
SET status = 'superseded'
WHERE incarnation_id = $1 AND kind = $2 AND status = 'active'
`

// Insert writes a new Warrant row. For active status MUST run inside the
// same tx as [SupersedeActive] of the previous active (otherwise the partial
// unique constraint breaks between supersede and insert).
//
// Pre-conditions: IncarnationID/VaultRef/SerialNumber non-empty; Kind valid;
// Fingerprint is 64 lower-hex; NotAfter is not zero.
//
// Returns: [ErrActiveExists] / [ErrFingerprintCollision] on UNIQUE,
// [ErrIncarnationNotFound] on FK.
func Insert(ctx context.Context, db ExecQueryRower, w *Warrant) error {
	if w == nil {
		return fmt.Errorf("cert: nil warrant")
	}
	if w.IncarnationID == "" {
		return fmt.Errorf("cert: incarnation_id is empty")
	}
	if !validKind(w.Kind) {
		return fmt.Errorf("cert: invalid kind %q", w.Kind)
	}
	if w.VaultRef == "" {
		return fmt.Errorf("cert: vault_ref is empty")
	}
	if w.SerialNumber == "" {
		return fmt.Errorf("cert: serial_number is empty")
	}
	if !ValidFingerprintFormat(w.Fingerprint) {
		return ErrInvalidFingerprint
	}
	if w.NotAfter.IsZero() {
		return fmt.Errorf("cert: not_after is zero")
	}
	if w.Status == "" {
		w.Status = StatusActive
	}
	if !validStatus(w.Status) {
		return fmt.Errorf("cert: invalid status %q", w.Status)
	}

	var issuedAtArg any
	if !w.IssuedAt.IsZero() {
		issuedAtArg = w.IssuedAt.UTC()
	}

	row := db.QueryRow(ctx, insertSQL,
		w.IncarnationID,
		string(w.Kind),
		w.VaultRef,
		w.SerialNumber,
		w.Fingerprint,
		w.NotAfter.UTC(),
		issuedAtArg,
		ptrArg(w.PKIMount),
		ptrArg(w.PKIRole),
		string(w.Status),
		ptrArg(w.IssuedByKID),
		ptrArg(w.LastRotationVoyageID),
		w.AutoRotate,
		durationArg(w.RotateThresholdOverride),
	)
	if err := row.Scan(&w.CertID, &w.IssuedAt); err != nil {
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
			case "warrant_active_by_incarnation_kind_idx":
				return fmt.Errorf("%w (constraint %s): %w", ErrActiveExists, pgErr.ConstraintName, err)
			}
			// fingerprint is not UNIQUE in the schema (unlike soul_seeds):
			// cert/key/ca of one incarnation have different fingerprints,
			// but global uniqueness is not required (service certs of
			// different incarnations are theoretically independent). Branch
			// kept for forward-compat if a UNIQUE index is added.
			return fmt.Errorf("cert: unique violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "warrant_incarnation_fk" {
				return fmt.Errorf("%w (constraint %s): %w", ErrIncarnationNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("cert: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("cert: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("cert: insert: %w", err)
}

// SelectActive returns the active row for (incarnation, kind), or
// [ErrNotFound].
func SelectActive(ctx context.Context, db ExecQueryRower, incarnationID string, kind Kind) (*Warrant, error) {
	row := db.QueryRow(ctx, selectActiveSQL, incarnationID, string(kind))
	w, err := scanWarrant(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return w, nil
}

// SupersedeActive moves the active row for a given (incarnation, kind) to
// superseded. No-op (rows=0) if there is no active (first issuance). Must be
// called inside the same tx as [Insert] of the new active.
func SupersedeActive(ctx context.Context, db ExecQueryRower, incarnationID string, kind Kind) error {
	if incarnationID == "" {
		return fmt.Errorf("cert: incarnation_id is empty")
	}
	if !validKind(kind) {
		return fmt.Errorf("cert: invalid kind %q", kind)
	}
	if _, err := db.Exec(ctx, supersedeActiveSQL, incarnationID, string(kind)); err != nil {
		return fmt.Errorf("cert: supersede active: %w", err)
	}
	return nil
}

// markStatusSQL — point status change of a row by cert_id (CAS by expected
// current status). WHERE status=$3 — optimistic barrier: transition to
// rotating (single-winner) and failed is done only from the expected
// previous status.
const markStatusSQL = `
UPDATE warrant
SET status = $2
WHERE cert_id = $1 AND status = $3
`

// MarkStatus atomically changes the status of row cert_id from `from` to
// `to` (CAS). Returns the number of affected rows: 0 = cert_id does not
// exist OR is no longer in status from (lost the race — another
// tick/instance grabbed it). Used for:
//   - transition active/failed → rotating (single-winner barrier at the
//     start of rotation);
//   - transition rotating → failed (the chain failed after acquisition).
func MarkStatus(ctx context.Context, db ExecQueryRower, certID string, from, to Status) (int64, error) {
	if certID == "" {
		return 0, fmt.Errorf("cert: cert_id is empty")
	}
	if !validStatus(from) || !validStatus(to) {
		return 0, fmt.Errorf("cert: invalid status transition %q → %q", from, to)
	}
	tag, err := db.Exec(ctx, markStatusSQL, certID, string(to), string(from))
	if err != nil {
		return 0, fmt.Errorf("cert: mark status %s→%s: %w", from, to, err)
	}
	return tag.RowsAffected(), nil
}

func scanWarrant(row pgx.Row) (*Warrant, error) {
	var (
		w             Warrant
		kindStr       string
		statusStr     string
		thresholdOver *time.Duration
	)
	err := row.Scan(
		&w.CertID,
		&w.IncarnationID,
		&kindStr,
		&w.VaultRef,
		&w.SerialNumber,
		&w.Fingerprint,
		&w.NotAfter,
		&w.IssuedAt,
		&w.PKIMount,
		&w.PKIRole,
		&statusStr,
		&w.IssuedByKID,
		&w.LastRotationVoyageID,
		&w.AutoRotate,
		&thresholdOver,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("cert: scan: %w", err)
	}
	w.Kind = Kind(kindStr)
	w.Status = Status(statusStr)
	w.RotateThresholdOverride = thresholdOver
	return &w, nil
}

// ptrArg unwraps *string into a nil-able arg for pgx (nil → NULL).
func ptrArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// durationArg unwraps *time.Duration into an INTERVAL arg (nil → NULL). pgx
// maps time.Duration to PG INTERVAL natively.
func durationArg(p *time.Duration) any {
	if p == nil {
		return nil
	}
	return *p
}

// RegisterActive atomically registers new active material for (incarnation,
// kind): supersede the previous active (if any) + insert the new one — in
// one tx, so the partial unique `warrant_active_by_incarnation_kind_idx`
// isn't violated between supersede and insert. Mutates w (CertID/IssuedAt
// after insert).
//
// Used by the keeper-side core module `core.cert.registered` (E1,
// coremod/cert) and the Reaper rule `rotate_due_certs` (on rotation the new
// cert is written the same way inside its own tx).
func RegisterActive(ctx context.Context, pool TxBeginner, w *Warrant) error {
	if w == nil {
		return fmt.Errorf("cert: nil warrant")
	}
	if w.Status == "" {
		w.Status = StatusActive
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("cert: begin register-active tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := SupersedeActive(ctx, tx, w.IncarnationID, w.Kind); err != nil {
		return err
	}
	if err := Insert(ctx, tx, w); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("cert: commit register-active tx: %w", err)
	}
	return nil
}
