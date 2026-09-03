package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/registrylabel"
)

// Sentinel errors for the CRUD layer. Handler side (Cloud.CRUD.b) maps:
//   - ErrProviderAlreadyExists -> 409 provider-already-exists.
//   - ErrProviderNotFound      -> 404 not-found.
var (
	ErrProviderAlreadyExists = errors.New("provider: id already exists")
	ErrProviderNotFound      = errors.New("provider: id not found")
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
INSERT INTO providers (id, type, region, credentials_ref, created_by_aid, fqdn_suffix, label)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at
`

const selectColumns = `id, type, region, credentials_ref, created_by_aid, created_at, fqdn_suffix, label`

// updateLabelSQL replaces the display caption of one row ([ADR-0085]). It touches
// `label` and nothing else — the PK is not in the SET list, because the
// identifier is immutable and no rename operation exists anywhere.
const updateLabelSQL = `
UPDATE providers AS x
SET label = $2
FROM providers AS old
WHERE x.id = $1 AND old.id = x.id
RETURNING old.label
`

const selectByIDSQL = `
SELECT ` + selectColumns + `
FROM providers
WHERE id = $1
`

const deleteSQL = `DELETE FROM providers WHERE id = $1`

// Insert inserts a new Provider.
//
// Pre-conditions:
//   - p.Name / p.Type match [IDPattern];
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
	if !ValidID(p.ID) {
		return fmt.Errorf("provider: invalid id %q (must match %s)", p.ID, IDPattern)
	}
	if !ValidID(p.Type) {
		return fmt.Errorf("provider: invalid type %q (must match %s)", p.Type, IDPattern)
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
	// The label is canonicalised, never validated: free text is the point
	// ([ADR-0085]). Blank collapses to NULL so "absent" has one spelling.
	p.Label = registrylabel.Normalize(p.Label)
	var label any
	if p.Label != nil {
		label = *p.Label
	}

	row := db.QueryRow(ctx, insertSQL,
		p.ID, p.Type, p.Region, p.CredentialsRef, createdByAID, fqdnSuffix, label,
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

// SelectByID reads a Provider by PK. [ErrProviderNotFound] on pgx.ErrNoRows.
func SelectByID(ctx context.Context, db ExecQueryRower, id string) (*Provider, error) {
	row := db.QueryRow(ctx, selectByIDSQL, id)
	return scanProvider(row)
}

func scanProvider(row pgx.Row) (*Provider, error) {
	var (
		p            Provider
		createdByAID *string
		fqdnSuffix   *string
		label        *string
	)
	err := row.Scan(
		&p.ID,
		&p.Type,
		&p.Region,
		&p.CredentialsRef,
		&createdByAID,
		&p.CreatedAt,
		&fqdnSuffix,
		&label,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("provider: scan: %w", err)
	}
	p.CreatedByAID = createdByAID
	p.FQDNSuffix = fqdnSuffix
	p.Label = label
	return &p, nil
}

// UpdateLabel replaces the display caption of one Provider and returns the caption
// the row held BEFORE the write ([ADR-0085]). [ErrProviderNotFound] when the row is absent.
//
// label==nil (or blank, which [registrylabel.Normalize] collapses to nil) clears
// the caption back to NULL, and the consumer falls back to showing the name. The
// value is stored as given otherwise: capitals, spaces and punctuation are what
// the field is for, so there is no format check to fail.
//
// The previous value comes back from the SAME statement rather than from a read
// before it: the audit event records `{old_label, new_label}` (parity with
// `incarnation.traits_changed`, which carries old and new keys), and a separate
// read would let a concurrent edit make that pair describe a transition that
// never happened.
//
// The name argument addresses the row; it is never written. Nothing derived moves
// as a result of this call — see the package doc of
// keeper/internal/registrylabel.
func UpdateLabel(ctx context.Context, db ExecQueryRower, id string, label *string) (*string, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("provider: invalid id %q (must match %s)", id, IDPattern)
	}
	var v any
	if n := registrylabel.Normalize(label); n != nil {
		v = *n
	}
	var previous *string
	if err := db.QueryRow(ctx, updateLabelSQL, id, v).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProviderNotFound
		}
		return nil, fmt.Errorf("provider: update label: %w", err)
	}
	return previous, nil
}

// Delete removes a Provider by PK. [ErrProviderNotFound] when the row is absent
// (RowsAffected==0).
//
// FK profiles_provider_fk (ON DELETE RESTRICT, migration 020): deleting a Provider
// with dependent Profiles returns a wrapped FK violation ([ErrProviderHasProfiles]).
// Handler maps it to 409, symmetric to "delete impossible, dependencies exist".
func Delete(ctx context.Context, db ExecQueryRower, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("provider: invalid id %q (must match %s)", id, IDPattern)
	}
	tag, err := db.Exec(ctx, deleteSQL, id)
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
ORDER BY created_at DESC, id ASC
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
