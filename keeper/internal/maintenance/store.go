package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// windowColumns lists a window row's columns, each qualified with table when it
// is not empty, in the order [windowRow.dest] scans them. The columns of the
// kind a row is not are NULL (migration 136); the scalar ones read back as
// zero values.
func windowColumns(table string) string {
	p := ""
	if table != "" {
		p = table + "."
	}
	return fmt.Sprintf(`%[1]sid::text, %[1]sincarnation_id, %[1]skind, %[1]sdays, COALESCE(%[1]sstart_time, ''),
		COALESCE(%[1]sduration_minutes, 0), COALESCE(%[1]stimezone, ''), %[1]sstarts_at, %[1]sends_at,
		%[1]scomment, %[1]screated_at, %[1]supdated_at`, p)
}

var windowCols = windowColumns("")

const freezeCols = `id::text, incarnation_id, starts_at, ends_at, comment, created_at, updated_at`

// Load reads an incarnation's windows and freezes and compiles them with the
// default ([Rules.Calculator]). The one entry point for a consumer that only
// needs the answers.
func Load(ctx context.Context, db DB, incarnationID string, defaults DefaultsSource) (*Calculator, error) {
	r, err := LoadRulesOf(ctx, db, incarnationID, defaults)
	if err != nil {
		return nil, err
	}
	return r.Calculator()
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
	args := append([]any{incarnationID}, s.columns()...)
	row := db.QueryRow(ctx, `INSERT INTO incarnation_maintenance_windows
			(incarnation_id, kind, days, start_time, duration_minutes, timezone, starts_at, ends_at, comment)
		SELECT $1::text, $2::text, $3::text[], $4::text, $5::int, $6::text, $7::timestamptz, $8::timestamptz, $9::text
		WHERE (SELECT count(*) FROM incarnation_maintenance_windows WHERE incarnation_id = $1::text) < $10::int
		RETURNING `+windowCols,
		append(args, MaxWindowsPerIncarnation)...)
	w, err := scanWindow(row)
	return w, insertErr("window", err)
}

// columns is a normalized s as the kind, days, start_time, duration_minutes,
// timezone, starts_at, ends_at and comment columns hold it: NULL in each column
// of the other kind, which the CHECK of migration 136 requires.
func (s WindowSpec) columns() []any {
	if s.Kind == KindOnce {
		return []any{s.Kind, nil, nil, nil, nil, s.StartsAt, s.EndsAt, s.Comment}
	}
	return []any{s.Kind, s.Days, s.StartTime, s.DurationMinutes, s.Timezone, nil, nil, s.Comment}
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
		SET kind = $3, days = $4, start_time = $5, duration_minutes = $6, timezone = $7,
			starts_at = $8, ends_at = $9, comment = $10, updated_at = now()
		FROM old WHERE w.id = old.id
		RETURNING `+windowColumns("old")+`, `+windowColumns("w"),
		append([]any{incarnationID, id}, s.columns()...)...)
	var b, a windowRow
	err = row.Scan(append(b.dest(), a.dest()...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Window{}, Window{}, ErrNotFound
	}
	if err != nil {
		return Window{}, Window{}, fmt.Errorf("maintenance: update window: %w", err)
	}
	return b.window(), a.window(), nil
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

// DeleteExpiredWindows deletes up to limit once windows that ended more than
// retention ago by the database clock, the longest-ended first, and returns them
// as they were. A weekly window never ends, so it is never one of them. A once
// window stops counting when it ends ([Calculator.HasWindows]); the retention is
// only how long a past one stays visible, so deleting it changes no answer.
func DeleteExpiredWindows(ctx context.Context, db DB, retention time.Duration, limit int) ([]Window, error) {
	if retention <= 0 {
		return nil, fmt.Errorf("maintenance: delete expired windows: retention must be positive, got %s", retention)
	}
	rows, err := db.Query(ctx, `DELETE FROM incarnation_maintenance_windows
		WHERE id IN (
			SELECT id FROM incarnation_maintenance_windows
			WHERE kind = 'once' AND ends_at < now() - make_interval(secs => $1)
			ORDER BY ends_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+windowCols, retention.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("maintenance: delete expired windows: %w", err)
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, fmt.Errorf("maintenance: delete expired windows: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("maintenance: delete expired windows: %w", err)
	}
	return out, nil
}

// DeleteExpiredFreezes is [DeleteExpiredWindows] for freezes: up to limit of
// those that ended more than retention ago.
func DeleteExpiredFreezes(ctx context.Context, db DB, retention time.Duration, limit int) ([]Freeze, error) {
	if retention <= 0 {
		return nil, fmt.Errorf("maintenance: delete expired freezes: retention must be positive, got %s", retention)
	}
	rows, err := db.Query(ctx, `DELETE FROM incarnation_freezes
		WHERE id IN (
			SELECT id FROM incarnation_freezes
			WHERE ends_at < now() - make_interval(secs => $1)
			ORDER BY ends_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+freezeCols, retention.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("maintenance: delete expired freezes: %w", err)
	}
	defer rows.Close()
	var out []Freeze
	for rows.Next() {
		f, err := scanFreeze(rows)
		if err != nil {
			return nil, fmt.Errorf("maintenance: delete expired freezes: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("maintenance: delete expired freezes: %w", err)
	}
	return out, nil
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

// windowRow is the scan target of [windowColumns]: a once window's bounds are
// NULL on a weekly row.
type windowRow struct {
	w                Window
	startsAt, endsAt *time.Time
}

func (r *windowRow) dest() []any {
	return []any{&r.w.ID, &r.w.IncarnationID, &r.w.Kind, &r.w.Days, &r.w.StartTime, &r.w.DurationMinutes,
		&r.w.Timezone, &r.startsAt, &r.endsAt, &r.w.Comment, &r.w.CreatedAt, &r.w.UpdatedAt}
}

func (r *windowRow) window() Window {
	w := r.w
	if r.startsAt != nil {
		w.StartsAt = *r.startsAt
	}
	if r.endsAt != nil {
		w.EndsAt = *r.endsAt
	}
	return utcWindow(w)
}

func scanWindow(row pgx.Row) (Window, error) {
	var r windowRow
	err := row.Scan(r.dest()...)
	return r.window(), err
}

func scanFreeze(row pgx.Row) (Freeze, error) {
	var f Freeze
	err := row.Scan(&f.ID, &f.IncarnationID, &f.StartsAt, &f.EndsAt, &f.Comment, &f.CreatedAt, &f.UpdatedAt)
	return utcFreeze(f), err
}

func utcWindow(w Window) Window {
	w.StartsAt, w.EndsAt = w.StartsAt.UTC(), w.EndsAt.UTC()
	w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
	return w
}

func utcFreeze(f Freeze) Freeze {
	f.StartsAt, f.EndsAt = f.StartsAt.UTC(), f.EndsAt.UTC()
	f.CreatedAt, f.UpdatedAt = f.CreatedAt.UTC(), f.UpdatedAt.UTC()
	return f
}
