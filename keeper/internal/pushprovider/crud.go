package pushprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона маппит:
//   - ErrPushProviderAlreadyExists → 409 push-provider-already-exists.
//   - ErrPushProviderNotFound      → 404 not-found.
var (
	ErrPushProviderAlreadyExists = errors.New("pushprovider: name already exists")
	ErrPushProviderNotFound      = errors.New("pushprovider: name not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у.
// Симметрично provider/operator: unit-тесты ходят через fake без
// подъёма PG, production даёт реальный pool / Conn / Tx.
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

// ListFilter — параметры `GET /v1/push-providers`. Пустые поля = «без
// фильтра».
type ListFilter struct {
	// NamePattern — LIKE-форма префиксной фильтрации по имени
	// (например, "vault%"). Пустая строка → без фильтра.
	NamePattern string
}

const selectColumns = `name, params, created_at, updated_at, created_by_aid, updated_by_aid`

const insertSQL = `
INSERT INTO push_providers (name, params, created_by_aid)
VALUES ($1, $2, $3)
RETURNING created_at, updated_at
`

const selectByNameSQL = `
SELECT ` + selectColumns + `
FROM push_providers
WHERE name = $1
`

const updateSQL = `
UPDATE push_providers
SET params = $2,
    updated_at = NOW(),
    updated_by_aid = $3
WHERE name = $1
`

const deleteSQL = `DELETE FROM push_providers WHERE name = $1`

// Insert вставляет новую запись Push-Provider-а.
//
// Pre-conditions:
//   - p.Name матчит [NamePattern];
//   - p.CreatedByAID непустой (NOT NULL в схеме).
//
// Возврат:
//   - [ErrPushProviderAlreadyExists] на UNIQUE по PK.
//   - wrapped fmt.Errorf на FK-violation (`created_by_aid` ссылается на
//     несуществующий AID) и CHECK-violation (битый name-format).
func Insert(ctx context.Context, db ExecQueryRower, p *PushProvider) error {
	if p == nil {
		return fmt.Errorf("pushprovider: nil push provider")
	}
	if !ValidName(p.Name) {
		return fmt.Errorf("pushprovider: invalid name %q (must match %s)", p.Name, NamePattern)
	}
	if p.CreatedByAID == "" {
		return fmt.Errorf("pushprovider: created_by_aid is empty")
	}

	paramsBytes, err := marshalParams(p.Params)
	if err != nil {
		return fmt.Errorf("pushprovider: marshal params: %w", err)
	}

	row := db.QueryRow(ctx, insertSQL, p.Name, paramsBytes, p.CreatedByAID)
	if err := row.Scan(&p.CreatedAt, &p.UpdatedAt); err != nil {
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
				ErrPushProviderAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("pushprovider: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("pushprovider: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("pushprovider: insert: %w", err)
}

// SelectByName читает запись по PK. [ErrPushProviderNotFound] при pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*PushProvider, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanPushProvider(row)
}

func scanPushProvider(row pgx.Row) (*PushProvider, error) {
	var (
		p            PushProvider
		paramsBytes  []byte
		updatedByAID *string
	)
	err := row.Scan(
		&p.Name,
		&paramsBytes,
		&p.CreatedAt,
		&p.UpdatedAt,
		&p.CreatedByAID,
		&updatedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPushProviderNotFound
		}
		return nil, fmt.Errorf("pushprovider: scan: %w", err)
	}
	p.UpdatedByAID = updatedByAID
	if len(paramsBytes) > 0 {
		if err := json.Unmarshal(paramsBytes, &p.Params); err != nil {
			return nil, fmt.Errorf("pushprovider: unmarshal params: %w", err)
		}
	}
	return &p, nil
}

// Update заменяет params существующей записи (replace-семантика).
//
// Возврат [ErrPushProviderNotFound], если PK не найден (RowsAffected==0).
func Update(ctx context.Context, db ExecQueryRower, name string, params map[string]any, updatedByAID string) error {
	if !ValidName(name) {
		return fmt.Errorf("pushprovider: invalid name %q (must match %s)", name, NamePattern)
	}
	paramsBytes, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("pushprovider: marshal params: %w", err)
	}
	var updatedBy any
	if updatedByAID != "" {
		updatedBy = updatedByAID
	}
	tag, err := db.Exec(ctx, updateSQL, name, paramsBytes, updatedBy)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("pushprovider: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("pushprovider: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPushProviderNotFound
	}
	return nil
}

// Delete удаляет запись по PK. [ErrPushProviderNotFound], если запись
// отсутствует (RowsAffected==0).
func Delete(ctx context.Context, db ExecQueryRower, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("pushprovider: invalid name %q (must match %s)", name, NamePattern)
	}
	tag, err := db.Exec(ctx, deleteSQL, name)
	if err != nil {
		return fmt.Errorf("pushprovider: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPushProviderNotFound
	}
	return nil
}

// SelectAll возвращает страницу записей и общее количество (без
// offset/limit).
//
// Сортировка — `updated_at DESC, name ASC` (свежие выше; tie-break по
// имени, иначе пагинация неустойчива при одинаковом таймстемпе).
//
// Total и items получаются двумя запросами вне общей транзакции — total
// **eventually consistent**, симметрично provider.SelectAll.
func SelectAll(ctx context.Context, db ExecQueryRower, f ListFilter, offset, limit int) ([]*PushProvider, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("pushprovider: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("pushprovider: limit must be >= 1, got %d", limit)
	}

	whereSQL, args := buildWhere(f)

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM push_providers"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("pushprovider: count: %w", err)
	}

	listSQL := `SELECT ` + selectColumns + `
FROM push_providers` + whereSQL + `
ORDER BY updated_at DESC, name ASC
OFFSET $` + itoa(len(args)+1) + ` LIMIT $` + itoa(len(args)+2)
	args = append(args, offset, limit)

	rows, err := db.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("pushprovider: list query: %w", err)
	}
	defer rows.Close()

	out := make([]*PushProvider, 0, limit)
	for rows.Next() {
		p, err := scanPushProvider(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("pushprovider: list iter: %w", err)
	}
	return out, total, nil
}

func buildWhere(f ListFilter) (string, []any) {
	if f.NamePattern == "" {
		return "", nil
	}
	return " WHERE name LIKE $1", []any{f.NamePattern}
}

// marshalParams сериализует params в JSON-bytes для прямой подстановки
// в JSONB-колонку. nil → `{}` (схема несёт DEFAULT, но pgx требует не-nil
// для NOT NULL). Симметрично operator.marshalMetadata.
func marshalParams(params map[string]any) ([]byte, error) {
	if params == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(params)
}

// itoa — мелкий helper для построения $N плейсхолдеров без strconv-импорта.
// Симметрично operator.intToString. Только non-negative-ветка (offset/limit
// гарантированы non-negative-валидацией выше).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
