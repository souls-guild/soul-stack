package soulseed

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя.
var (
	// ErrSeedActiveExists — попытка вставить второй active-seed для того же
	// SID (partial unique `soul_seeds_active_by_sid_idx`). Caller (bootstrap
	// handler) обязан сначала Supersede старый, потом Insert новый — в
	// одной транзакции.
	ErrSeedActiveExists = errors.New("soulseed: active seed for SID already exists (call SupersedeBySID first)")

	// ErrSeedFingerprintCollision — fingerprint уже существует в реестре.
	// Де-факто невозможно (SHA-256 публичного ключа уникален), но constraint
	// держим явно. Маппится в 500 internal (повторный выпуск того же ключа
	// — bug Vault PKI / CSR-handling-а).
	ErrSeedFingerprintCollision = errors.New("soulseed: fingerprint already exists in registry")

	// ErrSeedSoulNotFound — INSERT ссылается на отсутствующий SID в souls.
	ErrSeedSoulNotFound = errors.New("soulseed: target SID not found in souls registry")

	// ErrSeedNotFound — SelectActiveBySID не нашёл active-seed.
	ErrSeedNotFound = errors.New("soulseed: no active seed for SID")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество интерфейса pgxpool.Pool.
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

// supersedeBySIDSQL — переводит все active-seed-ы данного SID в superseded.
// При нормальном инварианте `_active_by_sid_idx` затронет ровно одну строку
// (либо ноль — у нового Soul-а active ещё нет).
const supersedeBySIDSQL = `
UPDATE soul_seeds
SET status = 'superseded'
WHERE sid = $1 AND status = 'active'
`

// revokeSQL — отозвать конкретный seed (по seed_id). Используется операторской
// API ревокации. Старые superseded/expired не трогаем — реальная защита от
// отозванного клиента живёт на mTLS-уровне (CRL) + WHERE status='active'.
const revokeSQL = `
UPDATE soul_seeds
SET status = 'revoked',
    revocation_reason = $2
WHERE seed_id = $1 AND status IN ('active', 'superseded')
`

// Insert вписывает новый SoulSeed. ОБЯЗАТЕЛЬНО внутри транзакции вместе
// с [SupersedeBySID] (если был старый active), [soul.UpdateStatus] и
// [bootstraptoken.Burn].
//
// Pre-conditions:
//   - s.SID непустой;
//   - s.Fingerprint — 64 lower-hex (ValidFingerprintFormat);
//   - s.SerialNumber непустой;
//   - s.ExpiresAt > s.IssuedAt (если IssuedAt не zero; иначе PG возьмёт NOW()).
//
// Возврат:
//   - [ErrSeedActiveExists] на UNIQUE по `soul_seeds_active_by_sid_idx`.
//   - [ErrSeedFingerprintCollision] на UNIQUE по `soul_seeds_fingerprint_idx`.
//   - [ErrSeedSoulNotFound] на FK-violation по `soul_seeds_sid_fk`.
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

// SelectActiveBySID возвращает текущий active-seed для SID, либо
// [ErrSeedNotFound] если active-seed-а нет (новый Soul без bootstrap-а или
// revoked-Soul).
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

// SelectByFingerprint — lookup при mTLS handshake для CRL-проверки статуса.
// Возвращает [ErrSeedNotFound] если fingerprint не известен реестру.
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
		// pgx.ErrNoRows пробрасывается caller-у — у тех больше контекста
		// (active-select vs by-fingerprint) для маппинга в нужный sentinel.
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

// SupersedeBySID переводит существующий active-seed данного SID в
// `superseded`. No-op (rows-affected = 0), если active-seed-а нет —
// допустимо при первом онбординге Soul-а.
//
// Должна вызываться внутри той же транзакции, что [Insert] нового active-seed-а,
// иначе partial unique нарушится между моментами supersede и insert.
func SupersedeBySID(ctx context.Context, db ExecQueryRower, sid string) error {
	if sid == "" {
		return fmt.Errorf("soulseed: sid is empty")
	}
	if _, err := db.Exec(ctx, supersedeBySIDSQL, sid); err != nil {
		return fmt.Errorf("soulseed: supersede: %w", err)
	}
	return nil
}

// orphanActiveBySIDSQL — cascade-перевод active-seed в `orphaned`
// (ADR-017). WHERE status='active' — намеренно: revoked > orphaned,
// если seed уже отозван, перетирать не нужно (security-precedence).
// superseded/expired в `orphaned` тоже не переводим — они и так
// «исторические», и Reaper подберёт их по обычным правилам.
const orphanActiveBySIDSQL = `
UPDATE soul_seeds
SET status = 'orphaned'
WHERE sid = $1 AND status = 'active'
`

// OrphanActiveBySID переводит active-seed данного SID в `orphaned`.
// Используется keeper-side core-модулем `core.cloud.provisioned destroyed`
// (ADR-017 cascade) внутри общей PG-транзакции вместе с
// `soul.UpdateStatus(destroyed)` и `bootstraptoken.BurnAllForSID`.
//
// No-op (rows-affected = 0), если active-seed-а нет (push-хост / уже revoked /
// никогда не онбоардился) — допустимо.
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

// Revoke помечает конкретный seed как revoked с сохранением reason. Затрагивает
// и active, и superseded (отозвать ещё-не-устаревший superseded полезно при
// security-инциденте). Не трогает уже expired/revoked.
//
// Возвращает количество затронутых строк (0 = seed_id не существует или уже
// expired/revoked); caller дифференцирует через SelectByFingerprint, если важно.
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

// ListFilter — фильтр для [SelectAll].
type ListFilter struct {
	SID    string
	Status Status
}

// SelectAll возвращает историю seed-ов с применённым фильтром. Сортировка —
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
