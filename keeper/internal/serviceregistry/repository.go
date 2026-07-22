package serviceregistry

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecQueryRower — the narrow subset of pgxpool.Pool the repository needs.
// Symmetric with augur/provider: unit tests go through a fake without spinning up PG,
// production supplies a real pool / Conn / Tx.
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

// SQLSTATE codes for UNIQUE / FK / CHECK violations. Kept locally (like
// augur/rbac) to avoid pulling pgerrcode into keeper.
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// --- service_registry -------------------------------------------------

const serviceColumns = `name, git, ref, refresh, created_by_aid, updated_by_aid, created_at, updated_at`

const insertServiceSQL = `
INSERT INTO service_registry (name, git, ref, refresh, created_by_aid, updated_by_aid)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at
`

const selectServiceByNameSQL = `
SELECT ` + serviceColumns + `
FROM service_registry
WHERE name = $1
`

const updateServiceSQL = `
UPDATE service_registry
SET git = $2, ref = $3, refresh = $4, updated_by_aid = $5, updated_at = NOW()
WHERE name = $1
RETURNING created_at, updated_at
`

const deleteServiceSQL = `DELETE FROM service_registry WHERE name = $1`

// InsertService inserts a new Service record and fills CreatedAt/UpdatedAt
// from RETURNING. Field validation happens at the service layer (service.go) BEFORE the call.
//
// Returns:
//   - [ErrAlreadyExists]    — UNIQUE on PK service_registry.name (23505);
//   - [ErrOperatorNotFound] — FK violation on created_by_aid/updated_by_aid;
//   - wrapped fmt.Errorf for CHECK violation / other.
func InsertService(ctx context.Context, db ExecQueryRower, e *ServiceEntry) error {
	if e == nil {
		return fmt.Errorf("serviceregistry: nil service entry")
	}
	row := db.QueryRow(ctx, insertServiceSQL,
		e.Name, e.Git, e.Ref, strOrNil(e.Refresh), strOrNil(e.CreatedByAID), strOrNil(e.UpdatedByAID),
	)
	if err := row.Scan(&e.CreatedAt, &e.UpdatedAt); err != nil {
		return mapServiceWriteError(err)
	}
	return nil
}

// GetService reads a Service record by PK. [ErrNotFound] on pgx.ErrNoRows.
func GetService(ctx context.Context, db ExecQueryRower, name string) (*ServiceEntry, error) {
	return scanService(db.QueryRow(ctx, selectServiceByNameSQL, name))
}

// ListServices returns all Service records. Sorted by `name ASC`
// (deterministic list order; the data volume is small, pagination isn't needed).
func ListServices(ctx context.Context, db ExecQueryRower) ([]*ServiceEntry, error) {
	const listSQL = `SELECT ` + serviceColumns + `
FROM service_registry
ORDER BY name ASC`
	rows, err := db.Query(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("serviceregistry: list services query: %w", wrapPgErr(err))
	}
	defer rows.Close()

	var out []*ServiceEntry
	for rows.Next() {
		e, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("serviceregistry: list services iter: %w", err)
	}
	return out, nil
}

// UpdateService replaces the mutable fields of a record (git/ref/refresh/updated_by_aid) and
// bumps updated_at. Name is the PK, it doesn't change (rename = delete+insert). Fills
// CreatedAt/UpdatedAt from RETURNING.
//
// Returns:
//   - [ErrNotFound]         — no row by PK (pgx.ErrNoRows from RETURNING);
//   - [ErrOperatorNotFound] — FK violation on updated_by_aid;
//   - wrapped fmt.Errorf for CHECK violation / other.
func UpdateService(ctx context.Context, db ExecQueryRower, e *ServiceEntry) error {
	if e == nil {
		return fmt.Errorf("serviceregistry: nil service entry")
	}
	row := db.QueryRow(ctx, updateServiceSQL,
		e.Name, e.Git, e.Ref, strOrNil(e.Refresh), strOrNil(e.UpdatedByAID),
	)
	if err := row.Scan(&e.CreatedAt, &e.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return mapServiceWriteError(err)
	}
	return nil
}

// DeleteService deletes a Service record by PK. [ErrNotFound] if the row didn't exist.
func DeleteService(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, deleteServiceSQL, name)
	if err != nil {
		return fmt.Errorf("serviceregistry: delete service: %w", wrapPgErr(err))
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanService(row pgx.Row) (*ServiceEntry, error) {
	var (
		e            ServiceEntry
		refresh      *string
		createdByAID *string
		updatedByAID *string
	)
	err := row.Scan(&e.Name, &e.Git, &e.Ref, &refresh, &createdByAID, &updatedByAID, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("serviceregistry: scan service: %w", err)
	}
	e.Refresh = refresh
	e.CreatedByAID = createdByAID
	e.UpdatedByAID = updatedByAID
	return &e, nil
}

// mapServiceWriteError maps pgx errors from the insert/update path to sentinels:
// UNIQUE → [ErrAlreadyExists], FK → [ErrOperatorNotFound], CHECK / other →
// wrapped (preserving the original via %w for errors.Is).
func mapServiceWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("serviceregistry: service CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("serviceregistry: write service: %w", err)
}

// --- keeper_settings --------------------------------------------------

const selectSettingSQL = `SELECT key, value, updated_by_aid, updated_at FROM keeper_settings WHERE key = $1`

// upsertSettingSQL — INSERT-or-UPDATE a setting by PK key. keeper_settings is a
// flat key-value store: set semantics are more natural than delete+insert, ON CONFLICT
// keeps repeated SetSetting calls idempotent.
const upsertSettingSQL = `
INSERT INTO keeper_settings (key, value, updated_by_aid, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (key) DO UPDATE
SET value = EXCLUDED.value, updated_by_aid = EXCLUDED.updated_by_aid, updated_at = NOW()
RETURNING updated_at
`

// GetSetting reads a setting by key. [ErrSettingNotFound] on pgx.ErrNoRows.
func GetSetting(ctx context.Context, db ExecQueryRower, key string) (*Setting, error) {
	var (
		s            Setting
		updatedByAID *string
	)
	err := db.QueryRow(ctx, selectSettingSQL, key).Scan(&s.Key, &s.Value, &updatedByAID, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSettingNotFound
		}
		return nil, fmt.Errorf("serviceregistry: scan setting: %w", err)
	}
	s.UpdatedByAID = updatedByAID
	return &s, nil
}

// SetSetting upserts a setting (key → value), bumping updated_at. updatedByAID
// is optional (nil → NULL). Fills UpdatedAt from RETURNING.
//
// Returns: [ErrOperatorNotFound] on FK violation for updated_by_aid; wrapped for
// CHECK / other.
func SetSetting(ctx context.Context, db ExecQueryRower, s *Setting) error {
	if s == nil {
		return fmt.Errorf("serviceregistry: nil setting")
	}
	err := db.QueryRow(ctx, upsertSettingSQL, s.Key, s.Value, strOrNil(s.UpdatedByAID)).Scan(&s.UpdatedAt)
	if err != nil {
		return mapSettingWriteError(err)
	}
	return nil
}

func mapSettingWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrOperatorNotFound, pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("serviceregistry: setting CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("serviceregistry: write setting: %w", err)
}

// strOrNil converts a *string into an args value: nil → nil (PG NULL), otherwise
// the dereferenced string. Without this, pgx sees a typed nil pointer instead of NULL.
func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// wrapPgErr adds the SQLSTATE to the message if the error is a pgconn.PgError.
// Simplifies distinguishing "table doesn't exist" (migration not applied) from
// transport failures (like rbac.wrapPgErr).
func wrapPgErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("pg %s: %w", pgErr.Code, err)
	}
	return err
}
