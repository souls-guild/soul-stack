package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors for the CRUD layer. Handler side (Cloud.CRUD.b) maps:
//   - ErrProviderAlreadyExists -> 409 provider-already-exists.
//   - ErrProviderNotFound      -> 404 not-found.
var (
	ErrProviderAlreadyExists = errors.New("provider: name already exists")
	ErrProviderNotFound      = errors.New("provider: name not found")
	// ErrProviderHasProfiles means deleting a Provider referenced by Profiles (FK
	// profiles_provider_fk ON DELETE RESTRICT, migration 020). Handler maps it to
	// 409 because delete is blocked by dependencies.
	ErrProviderHasProfiles = errors.New("provider: has dependent profiles")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower is the narrow pgxpool.Pool subset required by CRUD. Symmetric to
// incarnation/operator: unit tests use a fake without starting PG, while
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

// insertSQL is INSERT with RETURNING to fetch server-side created_at
// (DEFAULT NOW()) in one round trip.
const insertSQL = `
INSERT INTO providers (name, type, region, credentials_ref, created_by_aid, fqdn_suffix)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at
`

const selectColumns = `name, type, region, credentials_ref, created_by_aid, created_at, fqdn_suffix`

const selectByNameSQL = `
SELECT ` + selectColumns + `
FROM providers
WHERE name = $1
`

const deleteSQL = `DELETE FROM providers WHERE name = $1`

// Insert inserts a new Provider.
//
// Pre-conditions:
//   - p.Name / p.Type match [NamePattern];
//   - p.Region is non-empty;
//   - p.CredentialsRef passes [ValidCredentialsRef].
//
// Returns:
//   - [ErrProviderAlreadyExists] on UNIQUE by PK.
//   - wrapped fmt.Errorf on FK violation (`created_by_aid` references a missing
//     AID) and CHECK violation (name/type format).
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
	// fqdn_suffix is optional (self-onboard option T); if set, it must be a valid
	// DNS suffix, otherwise the predicted FQDN=SID will not pass soul.ValidSID.
	if p.FQDNSuffix != nil && !ValidFQDNSuffix(*p.FQDNSuffix) {
		return fmt.Errorf("provider: invalid fqdn_suffix %q (must match %s; use nil for none)",
			*p.FQDNSuffix, FQDNSuffixPattern)
	}

	var createdByAID any
	if p.CreatedByAID != nil {
		createdByAID = *p.CreatedByAID
	}
	var fqdnSuffix any
	if p.FQDNSuffix != nil {
		fqdnSuffix = *p.FQDNSuffix
	}

	row := db.QueryRow(ctx, insertSQL,
		p.Name, p.Type, p.Region, p.CredentialsRef, createdByAID, fqdnSuffix,
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

// SelectByName reads a Provider by PK. [ErrProviderNotFound] on pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*Provider, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanProvider(row)
}

func scanProvider(row pgx.Row) (*Provider, error) {
	var (
		p            Provider
		createdByAID *string
		fqdnSuffix   *string
	)
	err := row.Scan(
		&p.Name,
		&p.Type,
		&p.Region,
		&p.CredentialsRef,
		&createdByAID,
		&p.CreatedAt,
		&fqdnSuffix,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("provider: scan: %w", err)
	}
	p.CreatedByAID = createdByAID
	p.FQDNSuffix = fqdnSuffix
	return &p, nil
}

// Delete removes a Provider by PK. [ErrProviderNotFound] when the row is absent
// (RowsAffected==0).
//
// FK profiles_provider_fk (ON DELETE RESTRICT, migration 020): deleting a Provider
// with dependent Profiles returns a wrapped FK violation ([ErrProviderHasProfiles]).
// Handler maps it to 409, symmetric to "delete impossible, dependencies exist".
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

// SelectAll returns a page of Providers and the total count without offset/limit.
//
// Sort order is `created_at DESC, name ASC`: newer first, name as tie-breaker so
// pagination stays stable when timestamps match.
//
// Total and items are fetched by two queries outside one transaction, so total is
// eventually consistent, symmetric to incarnation.SelectAll.
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
