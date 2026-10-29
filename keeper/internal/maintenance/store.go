package maintenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of a pgx pool or transaction the store uses.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	// ErrNotFound — no window or freeze with that id on that incarnation. A
	// row of another incarnation is not found either: the incarnation in the
	// path is part of the address.
	ErrNotFound = errors.New("maintenance: not found")
	// ErrIncarnationNotFound — the incarnation vanished between the caller's
	// existence check and the insert (the foreign key refused it).
	ErrIncarnationNotFound = errors.New("maintenance: incarnation not found")
	// ErrLimitReached — the incarnation already holds the maximum number of
	// windows or freezes.
	ErrLimitReached = errors.New("maintenance: limit reached")
)

const pgForeignKeyViolation = "23503"

const windowCols = `id::text, incarnation_id, days, start_time, duration_minutes, timezone, comment, created_at, updated_at`

const freezeCols = `id::text, incarnation_id, starts_at, ends_at, comment, created_at, updated_at`

// Load reads an incarnation's windows and freezes and compiles them. The one
// entry point for a consumer that only needs the answers.
func Load(ctx context.Context, db DB, incarnationID string) (*Calculator, error) {
	windows, err := ListWindows(ctx, db, incarnationID)
	if err != nil {
		return nil, err
	}
	freezes, err := ListFreezes(ctx, db, incarnationID)
	if err != nil {
		return nil, err
	}
	return NewCalculator(windows, freezes)
}

// ListWindows returns the incarnation's windows in creation order.
func ListWindows(ctx context.Context, db DB, incarnationID string) ([]Window, error) {
	rows, err := db.Query(ctx, `SELECT `+windowCols+`
		FROM incarnation_maintenance_windows WHERE incarnation_id = $1
		ORDER BY created_at, id`, incarnationID)
	if err != nil {
		return nil, fmt.Errorf("maintenance: list windows: %w", err)
	}
	defer rows.Close()
	out := []Window{}
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, fmt.Errorf("maintenance: list windows: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("maintenance: list windows: %w", err)
	}
	return out, nil
}

// ListFreezes returns the incarnation's freezes, past ones included, by start.
func ListFreezes(ctx context.Context, db DB, incarnationID string) ([]Freeze, error) {
	rows, err := db.Query(ctx, `SELECT `+freezeCols+`
		FROM incarnation_freezes WHERE incarnation_id = $1
		ORDER BY starts_at, id`, incarnationID)
	if err != nil {
		return nil, fmt.Errorf("maintenance: list freezes: %w", err)
	}
	defer rows.Close()
	out := []Freeze{}
	for rows.Next() {
		f, err := scanFreeze(rows)
		if err != nil {
			return nil, fmt.Errorf("maintenance: list freezes: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("maintenance: list freezes: %w", err)
	}
	return out, nil
}

// InsertWindow validates s ([WindowSpec.Normalize]) and stores it. A refused
// field comes back as a [*ValidationError].
func InsertWindow(ctx context.Context, db DB, incarnationID string, s WindowSpec) (Window, error) {
	s, err := s.Normalize()
	if err != nil {
		return Window{}, err
	}
	row := db.QueryRow(ctx, `INSERT INTO incarnation_maintenance_windows
			(incarnation_id, days, start_time, duration_minutes, timezone, comment)
		SELECT $1::text, $2::text[], $3::text, $4::int, $5::text, $6::text
		WHERE (SELECT count(*) FROM incarnation_maintenance_windows WHERE incarnation_id = $1::text) < $7::int
		RETURNING `+windowCols,
		incarnationID, s.Days, s.StartTime, s.DurationMinutes, s.Timezone, s.Comment, MaxWindowsPerIncarnation)
	w, err := scanWindow(row)
	return w, insertErr("window", err)
}

// UpdateWindow validates s and replaces the window's fields with it, returning
// the row before and after, read off one statement so the pair describes a
// change that really happened.
func UpdateWindow(ctx context.Context, db DB, incarnationID, id string, s WindowSpec) (before, after Window, err error) {
	if s, err = s.Normalize(); err != nil {
		return Window{}, Window{}, err
	}
	row := db.QueryRow(ctx, `WITH old AS (
			SELECT * FROM incarnation_maintenance_windows
			WHERE incarnation_id = $1 AND id = $2::uuid FOR UPDATE)
		UPDATE incarnation_maintenance_windows w
		SET days = $3, start_time = $4, duration_minutes = $5, timezone = $6, comment = $7, updated_at = now()
		FROM old WHERE w.id = old.id
		RETURNING old.id::text, old.incarnation_id, old.days, old.start_time, old.duration_minutes,
			old.timezone, old.comment, old.created_at, old.updated_at,
			w.id::text, w.incarnation_id, w.days, w.start_time, w.duration_minutes,
			w.timezone, w.comment, w.created_at, w.updated_at`,
		incarnationID, id, s.Days, s.StartTime, s.DurationMinutes, s.Timezone, s.Comment)
	err = row.Scan(
		&before.ID, &before.IncarnationID, &before.Days, &before.StartTime, &before.DurationMinutes,
		&before.Timezone, &before.Comment, &before.CreatedAt, &before.UpdatedAt,
		&after.ID, &after.IncarnationID, &after.Days, &after.StartTime, &after.DurationMinutes,
		&after.Timezone, &after.Comment, &after.CreatedAt, &after.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Window{}, Window{}, ErrNotFound
	}
	if err != nil {
		return Window{}, Window{}, fmt.Errorf("maintenance: update window: %w", err)
	}
	return utcWindow(before), utcWindow(after), nil
}

// DeleteWindow removes a window and returns it as it was.
func DeleteWindow(ctx context.Context, db DB, incarnationID, id string) (Window, error) {
	row := db.QueryRow(ctx, `DELETE FROM incarnation_maintenance_windows
		WHERE incarnation_id = $1 AND id = $2::uuid RETURNING `+windowCols, incarnationID, id)
	w, err := scanWindow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Window{}, ErrNotFound
	}
	if err != nil {
		return Window{}, fmt.Errorf("maintenance: delete window: %w", err)
	}
	return w, nil
}

// InsertFreeze validates s ([FreezeSpec.Validate]) and stores it.
func InsertFreeze(ctx context.Context, db DB, incarnationID string, s FreezeSpec) (Freeze, error) {
	if err := s.Validate(); err != nil {
		return Freeze{}, err
	}
	row := db.QueryRow(ctx, `INSERT INTO incarnation_freezes (incarnation_id, starts_at, ends_at, comment)
		SELECT $1::text, $2::timestamptz, $3::timestamptz, $4::text
		WHERE (SELECT count(*) FROM incarnation_freezes WHERE incarnation_id = $1::text) < $5::int
		RETURNING `+freezeCols,
		incarnationID, s.StartsAt, s.EndsAt, s.Comment, MaxFreezesPerIncarnation)
	f, err := scanFreeze(row)
	return f, insertErr("freeze", err)
}

// UpdateFreeze validates s and replaces the freeze's fields with it, returning
// the row before and after.
func UpdateFreeze(ctx context.Context, db DB, incarnationID, id string, s FreezeSpec) (before, after Freeze, err error) {
	if err = s.Validate(); err != nil {
		return Freeze{}, Freeze{}, err
	}
	row := db.QueryRow(ctx, `WITH old AS (
			SELECT * FROM incarnation_freezes
			WHERE incarnation_id = $1 AND id = $2::uuid FOR UPDATE)
		UPDATE incarnation_freezes f
		SET starts_at = $3, ends_at = $4, comment = $5, updated_at = now()
		FROM old WHERE f.id = old.id
		RETURNING old.id::text, old.incarnation_id, old.starts_at, old.ends_at, old.comment,
			old.created_at, old.updated_at,
			f.id::text, f.incarnation_id, f.starts_at, f.ends_at, f.comment, f.created_at, f.updated_at`,
		incarnationID, id, s.StartsAt, s.EndsAt, s.Comment)
	err = row.Scan(
		&before.ID, &before.IncarnationID, &before.StartsAt, &before.EndsAt, &before.Comment,
		&before.CreatedAt, &before.UpdatedAt,
		&after.ID, &after.IncarnationID, &after.StartsAt, &after.EndsAt, &after.Comment,
		&after.CreatedAt, &after.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Freeze{}, Freeze{}, ErrNotFound
	}
	if err != nil {
		return Freeze{}, Freeze{}, fmt.Errorf("maintenance: update freeze: %w", err)
	}
	return utcFreeze(before), utcFreeze(after), nil
}

// DeleteFreeze removes a freeze and returns it as it was.
func DeleteFreeze(ctx context.Context, db DB, incarnationID, id string) (Freeze, error) {
	row := db.QueryRow(ctx, `DELETE FROM incarnation_freezes
		WHERE incarnation_id = $1 AND id = $2::uuid RETURNING `+freezeCols, incarnationID, id)
	f, err := scanFreeze(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Freeze{}, ErrNotFound
	}
	if err != nil {
		return Freeze{}, fmt.Errorf("maintenance: delete freeze: %w", err)
	}
	return f, nil
}

// insertErr maps the two ways a capped insert returns nothing useful: no row
// means the count guard refused it, a foreign-key violation means the
// incarnation is gone.
func insertErr(what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLimitReached
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return ErrIncarnationNotFound
	}
	return fmt.Errorf("maintenance: insert %s: %w", what, err)
}

func scanWindow(row pgx.Row) (Window, error) {
	var w Window
	err := row.Scan(&w.ID, &w.IncarnationID, &w.Days, &w.StartTime, &w.DurationMinutes,
		&w.Timezone, &w.Comment, &w.CreatedAt, &w.UpdatedAt)
	return utcWindow(w), err
}

func scanFreeze(row pgx.Row) (Freeze, error) {
	var f Freeze
	err := row.Scan(&f.ID, &f.IncarnationID, &f.StartsAt, &f.EndsAt, &f.Comment, &f.CreatedAt, &f.UpdatedAt)
	return utcFreeze(f), err
}

func utcWindow(w Window) Window {
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w
}

func utcFreeze(f Freeze) Freeze {
	f.StartsAt, f.EndsAt = f.StartsAt.UTC(), f.EndsAt.UTC()
	f.CreatedAt, f.UpdatedAt = f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	return f
}
