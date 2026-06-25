package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона (Cloud.CRUD.b) маппит:
//   - ErrProviderAlreadyExists → 409 provider-already-exists.
//   - ErrProviderNotFound      → 404 not-found.
var (
	ErrProviderAlreadyExists = errors.New("provider: name already exists")
	ErrProviderNotFound      = errors.New("provider: name not found")
	// ErrProviderHasProfiles — попытка удалить Provider, на который ссылаются
	// Profile-и (FK profiles_provider_fk ON DELETE RESTRICT, миграция 020).
	// Handler маппит в 409 (delete заблокирован зависимостями).
	ErrProviderHasProfiles = errors.New("provider: has dependent profiles")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у.
// Симметрично incarnation/operator: unit-тесты ходят через fake без подъёма
// PG, production даёт реальный pool / Conn / Tx.
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

// insertSQL — INSERT с RETURNING для получения server-side created_at
// (DEFAULT NOW()) одной round-trip-ой.
const insertSQL = `
INSERT INTO providers (name, type, region, credentials_ref, created_by_aid)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at
`

const selectColumns = `name, type, region, credentials_ref, created_by_aid, created_at`

const selectByNameSQL = `
SELECT ` + selectColumns + `
FROM providers
WHERE name = $1
`

const deleteSQL = `DELETE FROM providers WHERE name = $1`

// Insert вставляет новый Provider.
//
// Pre-conditions:
//   - p.Name / p.Type соответствуют [NamePattern];
//   - p.Region непустой;
//   - p.CredentialsRef проходит [ValidCredentialsRef].
//
// Возврат:
//   - [ErrProviderAlreadyExists] на UNIQUE по PK.
//   - wrapped fmt.Errorf на FK-violation (`created_by_aid` ссылается на
//     несуществующий AID) и CHECK-violation (name/type format).
func Insert(ctx context.Context, db ExecQueryRower, p *Provider) error {
	if p == nil {
		return fmt.Errorf("provider: nil provider")
	}
	if !ValidName(p.Name) {
		return fmt.Errorf("provider: invalid name %q (must match %s)", p.Name, NamePattern)
	}
	if !ValidName(p.Type) {
		return fmt.Errorf("provider: invalid type %q (must match %s)", p.Type, NamePattern)
	}
	if p.Region == "" {
		return fmt.Errorf("provider: region is empty")
	}
	if !ValidCredentialsRef(p.CredentialsRef) {
		return fmt.Errorf("provider: invalid credentials_ref %q (must start with %q and carry a path)",
			p.CredentialsRef, CredentialsRefPrefix)
	}

	var createdByAID any
	if p.CreatedByAID != nil {
		createdByAID = *p.CreatedByAID
	}

	row := db.QueryRow(ctx, insertSQL,
		p.Name, p.Type, p.Region, p.CredentialsRef, createdByAID,
	)
	if err := row.Scan(&p.CreatedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrProviderAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("provider: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("provider: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("provider: insert: %w", err)
}

// SelectByName читает Provider по PK. [ErrProviderNotFound] при pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*Provider, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanProvider(row)
}

func scanProvider(row pgx.Row) (*Provider, error) {
	var (
		p            Provider
		createdByAID *string
	)
	err := row.Scan(
		&p.Name,
		&p.Type,
		&p.Region,
		&p.CredentialsRef,
		&createdByAID,
		&p.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("provider: scan: %w", err)
	}
	p.CreatedByAID = createdByAID
	return &p, nil
}

// Delete удаляет Provider по PK. [ErrProviderNotFound], если запись
// отсутствует (RowsAffected==0).
//
// FK profiles_provider_fk (ON DELETE RESTRICT, миграция 020): удаление
// Provider-а с зависимыми Profile-ями отдаёт wrapped FK-violation
// ([ErrProviderHasProfiles]) — handler маппит её в 409, симметрично
// «удаление невозможно, есть зависимости».
func Delete(ctx context.Context, db ExecQueryRower, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("provider: invalid name %q (must match %s)", name, NamePattern)
	}
	tag, err := db.Exec(ctx, deleteSQL, name)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("%w (constraint %s): %w",
				ErrProviderHasProfiles, pgErr.ConstraintName, err)
		}
		return fmt.Errorf("provider: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderNotFound
	}
	return nil
}

// SelectAll возвращает страницу Provider-ов и общее количество (без
// offset/limit).
//
// Сортировка — `created_at DESC, name ASC` (поздние выше; tie-break по имени,
// иначе пагинация неустойчива при одинаковом таймстемпе).
//
// Total и items получаются двумя запросами вне общей транзакции — total
// **eventually consistent**, симметрично incarnation.SelectAll.
func SelectAll(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Provider, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("provider: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("provider: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM providers").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("provider: count: %w", err)
	}

	const listSQL = `SELECT ` + selectColumns + `
FROM providers
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("provider: list query: %w", err)
	}
	defer rows.Close()

	var out []*Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("provider: list iter: %w", err)
	}
	return out, total, nil
}
