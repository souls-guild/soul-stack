package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel-ошибки CRUD-слоя. Handler-сторона (Cloud.CRUD.b) маппит:
//   - ErrProfileAlreadyExists → 409 profile-already-exists.
//   - ErrProfileNotFound      → 404 not-found.
//   - ErrProviderNotFound     → 422 unprocessable (ссылка на несуществующий
//     Provider в `provider`-поле; FK profiles_provider_fk).
var (
	ErrProfileAlreadyExists = errors.New("profile: name already exists")
	ErrProfileNotFound      = errors.New("profile: name not found")
	ErrProviderNotFound     = errors.New("profile: referenced provider not found")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// providerFKConstraint — имя FK-constraint-а `profiles.provider →
// providers(name)` из 020-миграции. Нарушение этого FK (несуществующий
// Provider) маппится в [ErrProviderNotFound]; нарушение остальных FK
// (created_by_aid) — в generic FK-error.
const providerFKConstraint = "profiles_provider_fk"

// ExecQueryRower — узкое подмножество pgxpool.Pool, нужное CRUD-у.
// Симметрично provider/incarnation/operator.
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

// insertSQL — INSERT с RETURNING для server-side created_at одной round-trip-ой.
const insertSQL = `
INSERT INTO profiles (name, provider, params, cloud_init, created_by_aid)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at
`

const selectColumns = `name, provider, params, cloud_init, created_by_aid, created_at`

const selectByNameSQL = `
SELECT ` + selectColumns + `
FROM profiles
WHERE name = $1
`

const deleteSQL = `DELETE FROM profiles WHERE name = $1`

// Insert вставляет новый Profile.
//
// Pre-conditions:
//   - p.Name соответствует [NamePattern];
//   - p.Provider соответствует [NamePattern] (имя существующего Provider-а;
//     существование проверяет FK).
//
// Возврат:
//   - [ErrProfileAlreadyExists] на UNIQUE по PK.
//   - [ErrProviderNotFound] на FK-violation по `provider` (Provider не
//     существует).
//   - wrapped fmt.Errorf на прочих FK-violation (`created_by_aid`) и
//     CHECK-violation (name format).
func Insert(ctx context.Context, db ExecQueryRower, p *Profile) error {
	if p == nil {
		return fmt.Errorf("profile: nil profile")
	}
	if !ValidName(p.Name) {
		return fmt.Errorf("profile: invalid name %q (must match %s)", p.Name, NamePattern)
	}
	if !ValidName(p.Provider) {
		return fmt.Errorf("profile: invalid provider %q (must match %s)", p.Provider, NamePattern)
	}

	paramsBytes, err := marshalJSONB(p.Params)
	if err != nil {
		return fmt.Errorf("profile: marshal params: %w", err)
	}
	var cloudInitArg any
	if p.CloudInit != nil {
		cloudInitArg = *p.CloudInit
	}
	var createdByAID any
	if p.CreatedByAID != nil {
		createdByAID = *p.CreatedByAID
	}

	row := db.QueryRow(ctx, insertSQL,
		p.Name, p.Provider, paramsBytes, cloudInitArg, createdByAID,
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
				ErrProfileAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == providerFKConstraint {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrProviderNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("profile: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("profile: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("profile: insert: %w", err)
}

// SelectByName читает Profile по PK. [ErrProfileNotFound] при pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*Profile, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanProfile(row)
}

func scanProfile(row pgx.Row) (*Profile, error) {
	var (
		p            Profile
		paramsBytes  []byte
		cloudInit    *string
		createdByAID *string
	)
	err := row.Scan(
		&p.Name,
		&p.Provider,
		&paramsBytes,
		&cloudInit,
		&createdByAID,
		&p.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProfileNotFound
		}
		return nil, fmt.Errorf("profile: scan: %w", err)
	}
	p.CloudInit = cloudInit
	p.CreatedByAID = createdByAID
	if p.Params, err = unmarshalJSONB(paramsBytes); err != nil {
		return nil, fmt.Errorf("profile: unmarshal params: %w", err)
	}
	return &p, nil
}

// Delete удаляет Profile по PK. [ErrProfileNotFound], если запись
// отсутствует (RowsAffected==0). Profile-ы — листовые записи (на них FK не
// ссылается), поэтому FK-violation-ветки delete-у не нужно.
func Delete(ctx context.Context, db ExecQueryRower, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("profile: invalid name %q (must match %s)", name, NamePattern)
	}
	tag, err := db.Exec(ctx, deleteSQL, name)
	if err != nil {
		return fmt.Errorf("profile: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProfileNotFound
	}
	return nil
}

// SelectAll возвращает страницу Profile-ей и общее количество (без
// offset/limit). Сортировка — `created_at DESC, name ASC`. Total
// **eventually consistent**, симметрично provider/incarnation.SelectAll.
func SelectAll(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Profile, int, error) {
	return selectPage(ctx, db, "", offset, limit)
}

// SelectByProvider возвращает страницу Profile-ей конкретного Provider-а
// (та же сортировка / семантика total). Имя Provider-а в фильтр НЕ
// валидируется: несуществующий Provider даёт пустую страницу, total=0.
func SelectByProvider(ctx context.Context, db ExecQueryRower, providerName string, offset, limit int) ([]*Profile, int, error) {
	return selectPage(ctx, db, providerName, offset, limit)
}

// selectPage — общая реализация SelectAll / SelectByProvider. Пустой
// providerName означает «без фильтра».
func selectPage(ctx context.Context, db ExecQueryRower, providerName string, offset, limit int) ([]*Profile, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("profile: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("profile: limit must be >= 1, got %d", limit)
	}

	var (
		where string
		args  []any
	)
	if providerName != "" {
		where = " WHERE provider = $1"
		args = append(args, providerName)
	}

	countSQL := "SELECT COUNT(*) FROM profiles" + where
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("profile: count: %w", err)
	}

	listSQL := "SELECT " + selectColumns + " FROM profiles" + where +
		fmt.Sprintf(" ORDER BY created_at DESC, name ASC OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("profile: list query: %w", err)
	}
	defer rows.Close()

	var out []*Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("profile: list iter: %w", err)
	}
	return out, total, nil
}

// marshalJSONB сериализует map в bytes для JSONB-колонки. nil → `{}`,
// симметрично incarnation.marshalJSONB.
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalJSONB парсит JSONB-bytes в map. Пустые байты / `null` → nil-map.
func unmarshalJSONB(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
