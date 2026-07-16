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

// Sentinel errors for the CRUD layer. Handler side (Cloud.CRUD.b) maps:
//   - ErrProfileAlreadyExists -> 409 profile-already-exists.
//   - ErrProfileNotFound      -> 404 not-found.
//   - ErrProviderNotFound     -> 422 unprocessable (reference to a missing
//     Provider in the `provider` field; FK profiles_provider_fk).
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

// providerFKConstraint is the FK constraint name `profiles.provider ->
// providers(name)` from migration 020. This FK violation (missing Provider) maps
// to [ErrProviderNotFound]; other FK violations (created_by_aid) map to a generic
// FK error.
const providerFKConstraint = "profiles_provider_fk"

// ExecQueryRower is the narrow pgxpool.Pool subset required by CRUD. Symmetric to
// provider/incarnation/operator.
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

// insertSQL is INSERT with RETURNING for server-side created_at in one round trip.
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

// Insert inserts a new Profile.
//
// Pre-conditions:
//   - p.Name matches [NamePattern];
//   - p.Provider matches [NamePattern] (name of an existing Provider; existence is
//     checked by FK).
//
// Returns:
//   - [ErrProfileAlreadyExists] on UNIQUE by PK.
//   - [ErrProviderNotFound] on FK violation by `provider` (Provider does not
//     exist).
//   - wrapped fmt.Errorf on other FK violations (`created_by_aid`) and CHECK
//     violation (name format).
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

// SelectByName reads a Profile by PK. [ErrProfileNotFound] on pgx.ErrNoRows.
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

// Delete removes a Profile by PK. [ErrProfileNotFound] when the row is absent
// (RowsAffected==0). Profiles are leaf records with no inbound FK references, so
// delete does not need an FK-violation branch.
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

// SelectAll returns a page of Profiles and the total count without offset/limit.
// Sort order is `created_at DESC, name ASC`. Total is eventually consistent,
// symmetric to provider/incarnation.SelectAll.
func SelectAll(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Profile, int, error) {
	return selectPage(ctx, db, "", offset, limit)
}

// SelectByProvider returns a page of Profiles for a concrete Provider (same sort /
// total semantics). Provider name in the filter is not validated: a missing
// Provider returns an empty page, total=0.
func SelectByProvider(ctx context.Context, db ExecQueryRower, providerName string, offset, limit int) ([]*Profile, int, error) {
	return selectPage(ctx, db, providerName, offset, limit)
}

// selectPage is the shared implementation of SelectAll / SelectByProvider. Empty
// providerName means no filter.
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

// marshalJSONB serializes a map into bytes for a JSONB column. nil -> `{}`,
// symmetric to incarnation.marshalJSONB.
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalJSONB parses JSONB bytes into a map. Empty bytes / `null` -> nil-map.
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
