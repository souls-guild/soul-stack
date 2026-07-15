package choir

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG codes (parity incarnation/voyage).
const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — a narrow subset of pgxpool.Pool for read operations and for
// working inside a transaction (pgx.Tx satisfies the same interface).
// Symmetric to incarnation.ExecQueryRower.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// TxBeginner — a narrow subset of pgxpool.Pool for transactional operations
// (FOR UPDATE → check → mutate → commit). Symmetric to incarnation.TxBeginner.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Compile-time checks.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
	_ TxBeginner     = (*pgxpool.Pool)(nil)
)

// ---------------------------------------------------------------------------
// Choir CRUD
// ---------------------------------------------------------------------------

const insertChoirSQL = `
INSERT INTO incarnation_choirs (
    incarnation_name, choir_name, description, min_size, max_size, created_by_aid
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at
`

// CreateChoir creates a new Choir in the incarnation. choir_name is validated
// against the format (parity with the CHECK in migration
// 060_create_choirs.up.sql) before hitting the DB; min/max get a sane-bounds
// check. No transaction needed — a single INSERT; the FK on incarnation(name)
// guarantees the incarnation exists (FK violation → [ErrIncarnationNotFound]),
// UNIQUE on the PK → [ErrChoirExists].
//
// Returns:
//   - [ErrInvalidChoirName]    — choir_name doesn't match the format.
//   - [ErrInvalidSizeBounds]   — min/max ≤ 0 or min > max.
//   - [ErrIncarnationNotFound] — incarnation_name doesn't exist (FK violation).
//   - [ErrChoirExists]         — Choir already exists (UNIQUE on PK).
func CreateChoir(ctx context.Context, db ExecQueryRower, c *Choir) error {
	if c == nil {
		return fmt.Errorf("choir: nil choir")
	}
	if c.IncarnationName == "" {
		return fmt.Errorf("choir: empty incarnation_name")
	}
	if !ValidChoirName(c.ChoirName) {
		return fmt.Errorf("%w: %q", ErrInvalidChoirName, c.ChoirName)
	}
	if err := validateSizeBounds(c.MinSize, c.MaxSize); err != nil {
		return err
	}

	row := db.QueryRow(ctx, insertChoirSQL,
		c.IncarnationName,
		c.ChoirName,
		nullStr(c.Description),
		nullInt(c.MinSize),
		nullInt(c.MaxSize),
		nullStr(c.CreatedByAID),
	)
	if err := row.Scan(&c.CreatedAt); err != nil {
		return mapChoirInsertError(err)
	}
	return nil
}

func mapChoirInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return ErrChoirExists
		case pgErrCodeForeignKeyViolation:
			// The only FK that can "miss" its target when inserting a Choir with a
			// valid created_by_aid is incarnation(name). created_by_aid has
			// ON DELETE SET NULL, but inserting a nonexistent AID also produces an
			// FK violation; disambiguate by constraint name.
			if pgErr.ConstraintName == "incarnation_choirs_incarnation_fk" {
				return ErrIncarnationNotFound
			}
			return fmt.Errorf("choir: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("choir: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("choir: insert choir: %w", err)
}

const selectChoirSQL = `
SELECT incarnation_name, choir_name, description, min_size, max_size,
       created_at, created_by_aid
FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
`

// GetChoir reads a Choir by its PK pair. [ErrChoirNotFound] if absent.
func GetChoir(ctx context.Context, db ExecQueryRower, incarnation, choirName string) (*Choir, error) {
	if incarnation == "" || choirName == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	c, err := scanChoir(db.QueryRow(ctx, selectChoirSQL, incarnation, choirName))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChoirNotFound
		}
		return nil, fmt.Errorf("choir: get choir: %w", err)
	}
	return c, nil
}

const listChoirsSQL = `
SELECT incarnation_name, choir_name, description, min_size, max_size,
       created_at, created_by_aid
FROM incarnation_choirs
WHERE incarnation_name = $1
ORDER BY choir_name
`

// ListChoirs returns all Choirs of the incarnation ordered by name. An empty
// list is not an error (an incarnation with no Choirs, or a nonexistent one —
// disambiguating is the caller's job via SelectByName, S-T3 handler).
func ListChoirs(ctx context.Context, db ExecQueryRower, incarnation string) ([]*Choir, error) {
	if incarnation == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name")
	}
	rows, err := db.Query(ctx, listChoirsSQL, incarnation)
	if err != nil {
		return nil, fmt.Errorf("choir: list choirs: %w", err)
	}
	defer rows.Close()

	var out []*Choir
	for rows.Next() {
		c, scanErr := scanChoir(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("choir: list choirs scan: %w", scanErr)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("choir: list choirs iter: %w", err)
	}
	return out, nil
}

const deleteChoirSQL = `
DELETE FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
`

// DeleteChoir deletes a Choir (cascading its Voices — ON DELETE CASCADE on
// incarnation_choir_voices). [ErrChoirNotFound] if the row didn't exist
// (RowsAffected == 0) — guards against a silent no-op on a typo'd name.
func DeleteChoir(ctx context.Context, db ExecQueryRower, incarnation, choirName string) error {
	if incarnation == "" || choirName == "" {
		return fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	tag, err := db.Exec(ctx, deleteChoirSQL, incarnation, choirName)
	if err != nil {
		return fmt.Errorf("choir: delete choir: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrChoirNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Voice CRUD
// ---------------------------------------------------------------------------

const selectChoirForUpdateSQL = `
SELECT 1 FROM incarnation_choirs
WHERE incarnation_name = $1 AND choir_name = $2
FOR UPDATE
`

const insertVoiceSQL = `
INSERT INTO incarnation_choir_voices (
    incarnation_name, choir_name, sid, role, position, added_by_aid
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING added_at
`

// AddVoice adds a Voice (a SID's membership in a Choir) atomically.
// Transaction: SELECT … FOR UPDATE on the Choir row (serializes concurrent
// AddVoice / DeleteChoir) → membership invariant validation (SID is already a
// member of the incarnation: `souls.coven[]` contains `incarnation_name`,
// ADR-044 item 3) → INSERT voice → commit.
//
// The membership invariant is checked with an explicit SELECT (the FK on
// souls only covers the SID's existence in the registry, NOT membership in
// this incarnation). A SID that exists in souls but doesn't carry the
// incarnation in its coven is rejected with [ErrNotMembers].
//
// Returns:
//   - [ErrChoirNotFound]  — Choir doesn't exist (no row under FOR UPDATE).
//   - [ErrNotMembers]     — SID is not a member of the incarnation (absent
//     from souls, OR coven doesn't contain incarnation_name).
//   - [ErrVoiceExists]    — a Voice for this SID already exists in this Choir.
func AddVoice(ctx context.Context, pool TxBeginner, v *Voice) error {
	if v == nil {
		return fmt.Errorf("choir: nil voice")
	}
	if v.IncarnationName == "" || v.ChoirName == "" || v.SID == "" {
		return fmt.Errorf("choir: empty incarnation_name, choir_name or sid")
	}
	if v.Position != nil && *v.Position < 0 {
		return fmt.Errorf("choir: position must be >= 0, got %d", *v.Position)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("choir: begin add-voice tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the Choir row: guarantees the Choir exists at INSERT time and won't
	// be removed by a concurrent DeleteChoir between the check and the write.
	var dummy int
	if err := tx.QueryRow(ctx, selectChoirForUpdateSQL, v.IncarnationName, v.ChoirName).Scan(&dummy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChoirNotFound
		}
		return fmt.Errorf("choir: lock choir: %w", err)
	}

	if err := validateMembership(ctx, tx, v.IncarnationName, []string{v.SID}); err != nil {
		return err
	}

	if err := tx.QueryRow(ctx, insertVoiceSQL,
		v.IncarnationName,
		v.ChoirName,
		v.SID,
		nullStr(v.Role),
		nullInt(v.Position),
		nullStr(v.AddedByAID),
	).Scan(&v.AddedAt); err != nil {
		return mapVoiceInsertError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("choir: commit add-voice tx: %w", err)
	}
	return nil
}

func mapVoiceInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return ErrVoiceExists
		case pgErrCodeForeignKeyViolation:
			// sid_fk and choir_fk were already checked earlier (membership + FOR
			// UPDATE); an FK violation here can only come from added_by_aid with a
			// nonexistent AID.
			return fmt.Errorf("choir: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("choir: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("choir: insert voice: %w", err)
}

const deleteVoiceSQL = `
DELETE FROM incarnation_choir_voices
WHERE incarnation_name = $1 AND choir_name = $2 AND sid = $3
`

// RemoveVoice deletes a Voice by its PK triple. [ErrVoiceNotFound] on
// RowsAffected==0 (guards against a silent no-op on a typo). No transaction
// needed — a single DELETE.
func RemoveVoice(ctx context.Context, db ExecQueryRower, incarnation, choirName, sid string) error {
	if incarnation == "" || choirName == "" || sid == "" {
		return fmt.Errorf("choir: empty incarnation_name, choir_name or sid")
	}
	tag, err := db.Exec(ctx, deleteVoiceSQL, incarnation, choirName, sid)
	if err != nil {
		return fmt.Errorf("choir: remove voice: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVoiceNotFound
	}
	return nil
}

const listVoicesSQL = `
SELECT incarnation_name, choir_name, sid, role, position, added_at, added_by_aid
FROM incarnation_choir_voices
WHERE incarnation_name = $1 AND choir_name = $2
ORDER BY position NULLS LAST, sid
`

// ListVoices returns the Voices of a Choir, ordered by position (NULL last),
// then by sid. An empty list is not an error (a Choir with no Voices).
func ListVoices(ctx context.Context, db ExecQueryRower, incarnation, choirName string) ([]*Voice, error) {
	if incarnation == "" || choirName == "" {
		return nil, fmt.Errorf("choir: empty incarnation_name or choir_name")
	}
	rows, err := db.Query(ctx, listVoicesSQL, incarnation, choirName)
	if err != nil {
		return nil, fmt.Errorf("choir: list voices: %w", err)
	}
	defer rows.Close()

	var out []*Voice
	for rows.Next() {
		v, scanErr := scanVoice(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("choir: list voices scan: %w", scanErr)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("choir: list voices iter: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Membership invariant + helpers
// ---------------------------------------------------------------------------

const membershipSQL = `
SELECT sid FROM souls
WHERE sid = ANY($1) AND $2 = ANY(coven)
`

// validateMembership checks the ADR-044 item 3 invariant: every SID is
// already a member of the incarnation (its `souls.coven[]` contains
// incarnation). Stricter than incarnation.validateSoulsExist (which only
// checks the SID exists in souls): here membership in THIS incarnation is
// required specifically. A single batch SELECT with the
// `$incarnation = ANY(coven)` predicate — not a per-SID round-trip.
//
// Missing (in first-occurrence order, stable for tests) includes SIDs absent
// from souls entirely, AND SIDs that exist but aren't members of the
// incarnation.
func validateMembership(ctx context.Context, db ExecQueryRower, incarnation string, sids []string) error {
	if len(sids) == 0 {
		return nil
	}
	// Dedup + preserve first-occurrence order for Missing.
	seen := make(map[string]struct{}, len(sids))
	uniq := make([]string, 0, len(sids))
	for _, sid := range sids {
		if _, ok := seen[sid]; ok {
			continue
		}
		seen[sid] = struct{}{}
		uniq = append(uniq, sid)
	}

	rows, err := db.Query(ctx, membershipSQL, uniq, incarnation)
	if err != nil {
		return fmt.Errorf("choir: membership query: %w", err)
	}
	defer rows.Close()

	members := make(map[string]struct{}, len(uniq))
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return fmt.Errorf("choir: membership scan: %w", err)
		}
		members[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("choir: membership iter: %w", err)
	}

	var missing []string
	for _, sid := range uniq {
		if _, ok := members[sid]; !ok {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		return &ErrNotMembers{Incarnation: incarnation, Missing: missing}
	}
	return nil
}

// validateSizeBounds — sane-bounds check on min/max at Choir creation (parity
// with the CHECK constraints in migration 060_create_choirs.up.sql; gives a
// typed error before hitting the DB).
func validateSizeBounds(minSize, maxSize *int) error {
	if minSize != nil && *minSize <= 0 {
		return fmt.Errorf("%w: min_size must be > 0, got %d", ErrInvalidSizeBounds, *minSize)
	}
	if maxSize != nil && *maxSize <= 0 {
		return fmt.Errorf("%w: max_size must be > 0, got %d", ErrInvalidSizeBounds, *maxSize)
	}
	if minSize != nil && maxSize != nil && *minSize > *maxSize {
		return fmt.Errorf("%w: min_size %d > max_size %d", ErrInvalidSizeBounds, *minSize, *maxSize)
	}
	return nil
}

func scanChoir(row pgx.Row) (*Choir, error) {
	var c Choir
	if err := row.Scan(
		&c.IncarnationName,
		&c.ChoirName,
		&c.Description,
		&c.MinSize,
		&c.MaxSize,
		&c.CreatedAt,
		&c.CreatedByAID,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

func scanVoice(row pgx.Row) (*Voice, error) {
	var v Voice
	if err := row.Scan(
		&v.IncarnationName,
		&v.ChoirName,
		&v.SID,
		&v.Role,
		&v.Position,
		&v.AddedAt,
		&v.AddedByAID,
	); err != nil {
		return nil, err
	}
	return &v, nil
}

// nullStr / nullInt — *T → any for pgx binding (nil → SQL NULL).
func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
