package augur

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона (OpenAPI/MCP, отдельный слайс)
// маппит на HTTP-коды:
//   - ErrOmenAlreadyExists → 409.
//   - ErrOmenNotFound      → 404.
//   - ErrRiteNotFound      → 404.
var (
	ErrOmenAlreadyExists = errors.New("augur: omen name already exists")
	ErrOmenNotFound      = errors.New("augur: omen name not found")
	ErrRiteNotFound      = errors.New("augur: rite id not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у. Симметрично
// provider/incarnation: unit-тесты ходят через fake без подъёма PG, production
// даёт реальный pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// --- Omen -------------------------------------------------------------

const omenInsertSQL = `
INSERT INTO omens (name, source_type, endpoint, auth_ref, created_by_aid)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at
`

const omenColumns = `name, source_type, endpoint, auth_ref, created_by_aid, created_at`

const omenSelectByNameSQL = `
SELECT ` + omenColumns + `
FROM omens
WHERE name = $1
`

// InsertOmen вставляет новый Omen.
//
// Pre-conditions (service-валидация):
//   - o.Name соответствует [NamePattern];
//   - o.SourceType ∈ closed enum;
//   - o.Endpoint непустой;
//   - o.AuthRef — корректный vault-ref ([ValidAuthRef]).
//
// Возврат: [ErrOmenAlreadyExists] на UNIQUE по PK; wrapped fmt.Errorf на
// FK-/CHECK-violation.
func InsertOmen(ctx context.Context, db ExecQueryRower, o *Omen) error {
	if o == nil {
		return fmt.Errorf("augur: nil omen")
	}
	if !ValidName(o.Name) {
		return fmt.Errorf("augur: invalid omen name %q (must match %s)", o.Name, NamePattern)
	}
	if !ValidSourceType(o.SourceType) {
		return fmt.Errorf("augur: invalid source_type %q (must be vault/prometheus/elk)", o.SourceType)
	}
	if o.Endpoint == "" {
		return fmt.Errorf("augur: omen endpoint is empty")
	}
	if !ValidAuthRef(o.AuthRef) {
		return fmt.Errorf("augur: invalid auth_ref %q (must be a vault-ref vault:<mount>/<path>)", o.AuthRef)
	}

	var createdByAID any
	if o.CreatedByAID != nil {
		createdByAID = *o.CreatedByAID
	}

	row := db.QueryRow(ctx, omenInsertSQL,
		o.Name, string(o.SourceType), o.Endpoint, o.AuthRef, createdByAID,
	)
	if err := row.Scan(&o.CreatedAt); err != nil {
		return mapOmenInsertError(err)
	}
	return nil
}

func mapOmenInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrOmenAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("augur: omen FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("augur: omen CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("augur: insert omen: %w", err)
}

// SelectOmenByName читает Omen по PK. [ErrOmenNotFound] при pgx.ErrNoRows.
func SelectOmenByName(ctx context.Context, db ExecQueryRower, name string) (*Omen, error) {
	return scanOmen(db.QueryRow(ctx, omenSelectByNameSQL, name))
}

func scanOmen(row pgx.Row) (*Omen, error) {
	var (
		o            Omen
		srcType      string
		createdByAID *string
	)
	err := row.Scan(&o.Name, &srcType, &o.Endpoint, &o.AuthRef, &createdByAID, &o.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOmenNotFound
		}
		return nil, fmt.Errorf("augur: scan omen: %w", err)
	}
	o.SourceType = SourceType(srcType)
	o.CreatedByAID = createdByAID
	return &o, nil
}

// SelectAllOmens возвращает страницу Omen-ов и общее количество.
//
// Сортировка — `created_at DESC, name ASC` (как provider.SelectAll). Total и
// items — двумя запросами вне общей транзакции (eventually consistent).
func SelectAllOmens(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Omen, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("augur: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("augur: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM omens").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("augur: count omens: %w", err)
	}

	const listSQL = `SELECT ` + omenColumns + `
FROM omens
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("augur: list omens query: %w", err)
	}
	defer rows.Close()

	var out []*Omen
	for rows.Next() {
		o, err := scanOmen(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("augur: list omens iter: %w", err)
	}
	return out, total, nil
}

// DeleteOmen удаляет Omen по PK. Все его Rite-ы уходят каскадом (ON DELETE
// CASCADE, augur.md §9). [ErrOmenNotFound] если строки не было.
func DeleteOmen(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM omens WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("augur: delete omen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOmenNotFound
	}
	return nil
}

// --- Rite -------------------------------------------------------------

const riteInsertSQL = `
INSERT INTO rites (omen, coven, sid, allow, delegate, token_ttl, token_num_uses, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at
`

const riteColumns = `id, omen, coven, sid, allow, delegate, token_ttl, token_num_uses, created_by_aid, created_at`

// InsertRite вставляет новый Rite. Перед записью резолвит Omen (тем же db) для
// service-валидации, которую БД-CHECK не покрывает:
//   - allow-shape по source_type Omen-а ([ValidateAllow]);
//   - token-поля только при delegate=true И source_type=vault
//     ([ValidateTokenFields] — вторая половина инварианта, ⇒vault через join);
//   - формат token_ttl ([config.ParseDuration] внутри ValidateTokenFields).
//
// XOR-инвариант субъекта проверяется и здесь ([ValidateSubjectXOR]), и БД-CHECK
// rites_subject_xor — defence in depth.
//
// Возврат: [ErrOmenNotFound] если Omen не существует; wrapped fmt.Errorf на
// FK-/CHECK-violation.
func InsertRite(ctx context.Context, db ExecQueryRower, r *Rite) error {
	if r == nil {
		return fmt.Errorf("augur: nil rite")
	}
	if r.Omen == "" {
		return fmt.Errorf("augur: rite omen is empty")
	}
	if err := ValidateSubjectXOR(r); err != nil {
		return err
	}

	omen, err := SelectOmenByName(ctx, db, r.Omen)
	if err != nil {
		return err // ErrOmenNotFound или обёрнутая scan-ошибка
	}
	if err := ValidateAllow(omen.SourceType, r.Allow); err != nil {
		return err
	}
	if err := ValidateTokenFields(omen.SourceType, r); err != nil {
		return err
	}

	var (
		coven, sid   any
		createdByAID any
		tokenTTL     any
		tokenNumUses any
	)
	if r.Coven != nil {
		coven = *r.Coven
	}
	if r.SID != nil {
		sid = *r.SID
	}
	if r.CreatedByAID != nil {
		createdByAID = *r.CreatedByAID
	}
	if r.TokenTTL != nil {
		tokenTTL = *r.TokenTTL
	}
	if r.TokenNumUses != nil {
		tokenNumUses = *r.TokenNumUses
	}

	row := db.QueryRow(ctx, riteInsertSQL,
		r.Omen, coven, sid, []byte(r.Allow), r.Delegate, tokenTTL, tokenNumUses, createdByAID,
	)
	if err := row.Scan(&r.ID, &r.CreatedAt); err != nil {
		return mapRiteInsertError(err)
	}
	return nil
}

func mapRiteInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("augur: rite FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("augur: rite CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("augur: insert rite: %w", err)
}

func scanRite(row pgx.Row) (*Rite, error) {
	var (
		r            Rite
		allow        []byte
		coven        *string
		sid          *string
		tokenTTL     *string
		tokenNumUses *int
		createdByAID *string
	)
	err := row.Scan(
		&r.ID, &r.Omen, &coven, &sid, &allow,
		&r.Delegate, &tokenTTL, &tokenNumUses, &createdByAID, &r.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRiteNotFound
		}
		return nil, fmt.Errorf("augur: scan rite: %w", err)
	}
	r.Allow = allow
	r.Coven = coven
	r.SID = sid
	r.TokenTTL = tokenTTL
	r.TokenNumUses = tokenNumUses
	r.CreatedByAID = createdByAID
	return &r, nil
}

func collectRites(rows pgx.Rows) ([]*Rite, error) {
	defer rows.Close()
	var out []*Rite
	for rows.Next() {
		r, err := scanRite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("augur: list rites iter: %w", err)
	}
	return out, nil
}

// SelectRitesByOmen возвращает все Rite-ы одного Omen-а (authorization §6,
// CRUD list-by-omen). Сортировка `created_at DESC, id ASC`.
func SelectRitesByOmen(ctx context.Context, db ExecQueryRower, omen string) ([]*Rite, error) {
	const sql = `SELECT ` + riteColumns + `
FROM rites
WHERE omen = $1
ORDER BY created_at DESC, id ASC`
	rows, err := db.Query(ctx, sql, omen)
	if err != nil {
		return nil, fmt.Errorf("augur: list rites by omen query: %w", err)
	}
	return collectRites(rows)
}

// SelectRitesBySubject возвращает Rite-ы, матчащие субъект запроса: sid-Rite-ы с
// rites.sid == sid ИЛИ coven-Rite-ы с rites.coven ∈ covens (authorization §6).
// Пустой covens допустим (тогда только sid-Rite-ы). Используется при резолве
// AugurRequest в отдельном слайсе. Сортировка `created_at DESC, id ASC`.
func SelectRitesBySubject(ctx context.Context, db ExecQueryRower, sid string, covens []string) ([]*Rite, error) {
	const sql = `SELECT ` + riteColumns + `
FROM rites
WHERE sid = $1 OR coven = ANY($2)
ORDER BY created_at DESC, id ASC`
	rows, err := db.Query(ctx, sql, sid, covens)
	if err != nil {
		return nil, fmt.Errorf("augur: list rites by subject query: %w", err)
	}
	return collectRites(rows)
}

// DeleteRite удаляет Rite по суррогатному PK. [ErrRiteNotFound] если строки не
// было.
func DeleteRite(ctx context.Context, db ExecQueryRower, id int64) error {
	tag, err := db.Exec(ctx, "DELETE FROM rites WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("augur: delete rite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRiteNotFound
	}
	return nil
}
