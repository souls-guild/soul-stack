package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/registrylabel"
)

// Sentinel errors for the CRUD layer. Handler side (Cloud.CRUD.b) maps:
//   - ErrProfileAlreadyExists -> 409 profile-already-exists.
//   - ErrProfileNotFound      -> 404 not-found.
//   - ErrProviderNotFound     -> 422 unprocessable (reference to a missing
//     Provider in the `provider` field; FK profiles_provider_fk).
var (
	ErrProfileAlreadyExists = errors.New("profile: id already exists")
	ErrProfileNotFound      = errors.New("profile: id not found")
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
INSERT INTO profiles (id, provider, params, cloud_init, created_by_aid, label)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at
`

const selectColumns = `id, provider, params, cloud_init, created_by_aid, created_at, label`

// updateLabelSQL replaces the display caption of one row ([ADR-0085]). It touches
// `label` and nothing else — the PK is not in the SET list, because the
// identifier is immutable and no rename operation exists anywhere.
const updateLabelSQL = `
UPDATE profiles AS x
SET label = $2
FROM profiles AS old
WHERE x.id = $1 AND old.id = x.id
RETURNING old.label
`

const selectByIDSQL = `
SELECT ` + selectColumns + `
FROM profiles
WHERE id = $1
`

const deleteSQL = `DELETE FROM profiles WHERE id = $1`

// Insert inserts a new Profile.
//
// Pre-conditions:
//   - p.Name matches [IDPattern];
//   - p.Provider matches [IDPattern] (name of an existing Provider; existence is
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
	if !ValidID(p.ID) {
		return fmt.Errorf("profile: invalid id %q (must match %s)", p.ID, IDPattern)
	}
	if !ValidID(p.Provider) {
		return fmt.Errorf("profile: invalid provider %q (must match %s)", p.Provider, IDPattern)
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

	// The label is canonicalised, never validated: free text is the point
	// ([ADR-0085]). Blank collapses to NULL so "absent" has one spelling.
	p.Label = registrylabel.Normalize(p.Label)
	var label any
	if p.Label != nil {
		label = *p.Label
	}

	row := db.QueryRow(ctx, insertSQL,
		p.ID, p.Provider, paramsBytes, cloudInitArg, createdByAID, label,
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

// SelectByID reads a Profile by PK. [ErrProfileNotFound] on pgx.ErrNoRows.
func SelectByID(ctx context.Context, db ExecQueryRower, id string) (*Profile, error) {
	row := db.QueryRow(ctx, selectByIDSQL, id)
	return scanProfile(row)
}

func scanProfile(row pgx.Row) (*Profile, error) {
	var (
		p            Profile
		paramsBytes  []byte
		cloudInit    *string
		createdByAID *string
		label        *string
	)
	err := row.Scan(
		&p.ID,
		&p.Provider,
		&paramsBytes,
		&cloudInit,
		&createdByAID,
		&p.CreatedAt,
		&label,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProfileNotFound
		}
		return nil, fmt.Errorf("profile: scan: %w", err)
	}
	p.CloudInit = cloudInit
	p.CreatedByAID = createdByAID
	p.Label = label
	if p.Params, err = unmarshalJSONB(paramsBytes); err != nil {
		return nil, fmt.Errorf("profile: unmarshal params: %w", err)
	}
	return &p, nil
}

// UpdateLabel replaces the display caption of one Profile and returns the caption
// the row held BEFORE the write ([ADR-0085]). [ErrProfileNotFound] when the row is absent.
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
		return nil, fmt.Errorf("profile: invalid id %q (must match %s)", id, IDPattern)
	}
	var v any
	if n := registrylabel.Normalize(label); n != nil {
		v = *n
	}
	var previous *string
	if err := db.QueryRow(ctx, updateLabelSQL, id, v).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProfileNotFound
		}
		return nil, fmt.Errorf("profile: update label: %w", err)
	}
	return previous, nil
}

// Delete removes a Profile by PK. [ErrProfileNotFound] when the row is absent
// (RowsAffected==0). Profiles are leaf records with no inbound FK references, so
// delete does not need an FK-violation branch.
func Delete(ctx context.Context, db ExecQueryRower, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("profile: invalid id %q (must match %s)", id, IDPattern)
	}
	tag, err := db.Exec(ctx, deleteSQL, id)
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
		fmt.Sprintf(" ORDER BY created_at DESC, id ASC OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
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
