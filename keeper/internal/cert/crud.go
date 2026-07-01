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

// Sentinel-ошибки CRUD-слоя.
var (
	// ErrActiveExists — попытка вставить второй active-материал для той же
	// (incarnation, kind) (partial unique `warrant_active_by_incarnation_kind_idx`).
	// Caller обязан сначала Supersede старый, потом Insert новый — в одной tx.
	ErrActiveExists = errors.New("cert: active warrant for (incarnation, kind) already exists (call SupersedeActive first)")

	// ErrFingerprintCollision — fingerprint уже в реестре. Де-факто невозможно
	// (SHA-256 публичного ключа уникален); constraint держим явно.
	ErrFingerprintCollision = errors.New("cert: fingerprint already exists in registry")

	// ErrIncarnationNotFound — INSERT ссылается на отсутствующую инкарнацию
	// (FK `warrant_incarnation_fk`).
	ErrIncarnationNotFound = errors.New("cert: target incarnation not found")

	// ErrNotFound — Select не нашёл строку.
	ErrNotFound = errors.New("cert: warrant not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool (симметрия
// soulseed.ExecQueryRower): unit-тесты через fake без PG, production — pool/
// Conn/Tx.
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

// TxBeginner — узкое подмножество pgxpool.Pool для транзакционных операций
// (supersede старого active + insert нового в одной tx). Симметрично
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

// supersedeActiveSQL переводит active-строку данной (incarnation, kind) в
// superseded. При нормальном инварианте затронет ровно одну строку (либо ноль).
const supersedeActiveSQL = `
UPDATE warrant
SET status = 'superseded'
WHERE incarnation_id = $1 AND kind = $2 AND status = 'active'
`

// Insert вписывает новую строку Warrant. Для active-status ОБЯЗАТЕЛЬНО внутри
// той же tx, что [SupersedeActive] предыдущего active (иначе partial unique
// нарушится между supersede и insert).
//
// Pre-conditions: IncarnationID/VaultRef/SerialNumber непустые; Kind валиден;
// Fingerprint — 64 lower-hex; NotAfter не zero.
//
// Возврат: [ErrActiveExists] / [ErrFingerprintCollision] на UNIQUE,
// [ErrIncarnationNotFound] на FK.
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
			// fingerprint не UNIQUE в схеме (в отличие от soul_seeds): у cert/key/ca
			// одной инкарнации fingerprint-ы разные, но глобальной уникальности не
			// требуем (сервисные серты разных инкарнаций теоретически независимы).
			// Ветка оставлена для forward-compat, если добавим UNIQUE-индекс.
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

// SelectActive возвращает active-строку для (incarnation, kind), либо
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

// SupersedeActive переводит active-строку данной (incarnation, kind) в
// superseded. No-op (rows=0), если active нет (первый выпуск). Должна
// вызываться внутри той же tx, что [Insert] нового active.
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

// markStatusSQL — точечная смена статуса строки по cert_id (CAS by expected
// current status). WHERE status=$3 — оптимистичный барьер: перевод rotating
// (single-winner) и failed делается только из ожидаемого предыдущего статуса.
const markStatusSQL = `
UPDATE warrant
SET status = $2
WHERE cert_id = $1 AND status = $3
`

// MarkStatus атомарно меняет статус строки cert_id с from на to (CAS). Возвращает
// число затронутых строк: 0 = cert_id не существует ИЛИ уже не в статусе from
// (гонку проиграли — другой тик/инстанс перехватил). Используется:
//   - переход active/failed → rotating (single-winner-барьер начала ротации);
//   - переход rotating → failed (цепаль упала после захвата).
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

// ptrArg разворачивает *string в nil-able arg для pgx (nil → NULL).
func ptrArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// durationArg разворачивает *time.Duration в INTERVAL-arg (nil → NULL). pgx
// маппит time.Duration в PG INTERVAL нативно.
func durationArg(p *time.Duration) any {
	if p == nil {
		return nil
	}
	return *p
}

// RegisterActive атомарно регистрирует новый active-материал для (incarnation,
// kind): supersede предыдущего active (если был) + insert нового — в одной tx,
// чтобы partial unique `warrant_active_by_incarnation_kind_idx` не нарушался
// между supersede и insert. Мутирует w (CertID/IssuedAt после insert).
//
// Используется keeper-side core-модулем `core.cert.registered` (E1,
// coremod/cert) и Reaper-правилом `rotate_due_certs` (при ротации новый серт
// вписывается тем же путём внутри своей tx).
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
