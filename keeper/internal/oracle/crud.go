package oracle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// Sentinel errors for the registry.
var (
	ErrVigilNotFound  = errors.New("oracle: vigil not found")
	ErrDecreeNotFound = errors.New("oracle: decree not found")
)

const vigilColumns = `name, coven, sid, interval_spec, check_addr, params, enabled, created_at, updated_at, created_by_aid`

// SelectActiveVigilsForSubject returns enabled Vigils active for the
// subject (sid-Vigil with vigils.sid == sid OR coven-Vigil with an
// intersection of vigils.coven ∩ covens). Resolves the set for VigilSnapshot on connect.
//
// An empty covens is allowed (then only sid-Vigils). Sorting by `name ASC` is
// a deterministic snapshot order (ReplaceAll on the Soul doesn't depend on
// the order, but stability simplifies tests/diagnostics).
func SelectActiveVigilsForSubject(ctx context.Context, db ExecQueryRower, sid string, covens []string) ([]*Vigil, error) {
	const sql = `SELECT ` + vigilColumns + `
FROM vigils
WHERE enabled AND (sid = $1 OR coven && $2)
ORDER BY name ASC`
	rows, err := db.Query(ctx, sql, sid, covens)
	if err != nil {
		return nil, fmt.Errorf("oracle: list vigils by subject query: %w", err)
	}
	return collectVigils(rows)
}

func collectVigils(rows pgx.Rows) ([]*Vigil, error) {
	defer rows.Close()
	var out []*Vigil
	for rows.Next() {
		v, err := scanVigil(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oracle: list vigils iter: %w", err)
	}
	return out, nil
}

func scanVigil(row pgx.Row) (*Vigil, error) {
	v := &Vigil{}
	err := row.Scan(
		&v.Name, &v.Coven, &v.SID, &v.IntervalSpec, &v.CheckAddr,
		&v.Params, &v.Enabled, &v.CreatedAt, &v.UpdatedAt, &v.CreatedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrVigilNotFound
		}
		return nil, fmt.Errorf("oracle: scan vigil: %w", err)
	}
	return v, nil
}

// SelectVigilByName reads a Vigil by PK. [ErrVigilNotFound] on pgx.ErrNoRows.
func SelectVigilByName(ctx context.Context, db ExecQueryRower, name string) (*Vigil, error) {
	const sql = `SELECT ` + vigilColumns + `
FROM vigils
WHERE name = $1`
	return scanVigil(db.QueryRow(ctx, sql, name))
}

// SelectAllVigils returns a page of Vigils and the total count (sort
// created_at DESC, name ASC — symmetric with [augur.SelectAllOmens]). Total and
// items — two queries outside a shared transaction (eventually consistent).
func SelectAllVigils(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Vigil, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("oracle: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("oracle: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM vigils").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("oracle: count vigils: %w", err)
	}

	const listSQL = `SELECT ` + vigilColumns + `
FROM vigils
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("oracle: list vigils query: %w", err)
	}
	out, err := collectVigils(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// DeleteVigil deletes a Vigil by PK. [ErrVigilNotFound] if the row didn't exist.
// Decrees are NOT cascaded: decrees.on_beacon is a text reference without an FK
// (Decree is a managed registry, it survives Vigil recreation); deleting a Vigil
// merely stops handing it out in VigilSnapshot.
func DeleteVigil(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM vigils WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("oracle: delete vigil: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVigilNotFound
	}
	return nil
}

const decreeColumns = `name, on_beacon, where_cel, subject_coven, subject_sid, incarnation_name, action_scenario, action_input, cooldown, enabled, created_at, updated_at, created_by_aid`

// SelectDecreesByBeacon returns enabled Decrees reacting to the given
// Vigil (decrees.on_beacon == beacon). Hot path of the match flow: for every
// Portent, Oracle runs this SELECT, then filters by subject + where-CEL +
// cooldown (see [Match]). Default-deny: an empty result → the event is ignored.
//
// Sorting by `name ASC` is a deterministic processing order for multiple
// matching Decrees on one event.
func SelectDecreesByBeacon(ctx context.Context, db ExecQueryRower, beacon string) ([]*Decree, error) {
	const sql = `SELECT ` + decreeColumns + `
FROM decrees
WHERE enabled AND on_beacon = $1
ORDER BY name ASC`
	rows, err := db.Query(ctx, sql, beacon)
	if err != nil {
		return nil, fmt.Errorf("oracle: list decrees by beacon query: %w", err)
	}
	return collectDecrees(rows)
}

func collectDecrees(rows pgx.Rows) ([]*Decree, error) {
	defer rows.Close()
	var out []*Decree
	for rows.Next() {
		d, err := scanDecree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oracle: list decrees iter: %w", err)
	}
	return out, nil
}

func scanDecree(row pgx.Row) (*Decree, error) {
	d := &Decree{}
	err := row.Scan(
		&d.Name, &d.OnBeacon, &d.WhereCEL, &d.SubjectCoven, &d.SubjectSID,
		&d.IncarnationName, &d.ActionScenario, &d.ActionInput, &d.Cooldown,
		&d.Enabled, &d.CreatedAt, &d.UpdatedAt, &d.CreatedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDecreeNotFound
		}
		return nil, fmt.Errorf("oracle: scan decree: %w", err)
	}
	return d, nil
}

// SelectDecreeByName reads a Decree by PK. [ErrDecreeNotFound] on pgx.ErrNoRows.
func SelectDecreeByName(ctx context.Context, db ExecQueryRower, name string) (*Decree, error) {
	const sql = `SELECT ` + decreeColumns + `
FROM decrees
WHERE name = $1`
	return scanDecree(db.QueryRow(ctx, sql, name))
}

// SelectAllDecrees returns a page of Decrees and the total count (sort
// created_at DESC, name ASC). Total and items — two queries outside a shared
// transaction (eventually consistent).
func SelectAllDecrees(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Decree, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("oracle: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("oracle: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM decrees").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("oracle: count decrees: %w", err)
	}

	const listSQL = `SELECT ` + decreeColumns + `
FROM decrees
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("oracle: list decrees query: %w", err)
	}
	out, err := collectDecrees(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// DeleteDecree deletes a Decree by PK. Its cooldown state in `oracle_fires` is
// removed by cascade (ON DELETE CASCADE, migration 041). [ErrDecreeNotFound] if
// the row didn't exist.
func DeleteDecree(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM decrees WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("oracle: delete decree: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDecreeNotFound
	}
	return nil
}

// LastFiredAt returns the time of the last fire for the (decree, subject) pair
// from `oracle_fires` (cooldown state, ADR-030(a)). (zero, false, nil) means the
// pair hasn't fired yet (no row): cooldown is not active. Used by
// [WithinCooldown].
func LastFiredAt(ctx context.Context, db ExecQueryRower, decree, subject string) (time.Time, bool, error) {
	const sql = `SELECT fired_at FROM oracle_fires WHERE decree = $1 AND subject = $2`
	var firedAt time.Time
	err := db.QueryRow(ctx, sql, decree, subject).Scan(&firedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("oracle: last fired query: %w", err)
	}
	return firedAt, true, nil
}

// RecordFire records the fire of the (decree, subject) pair in `oracle_fires`
// (cooldown state, ADR-030(a)). UPSERT: one row per pair, fired_at
// is updated to now (NOT append-only — the table stays bounded by the
// number of unique pairs). Called AFTER successfully enqueuing the scenario.
//
// firedAt is the moment of firing (the caller passes a single timestamp,
// consistent with the cooldown check and the audit). An FK violation on a
// missing decree is mapped to a wrapped error (a caller programming error:
// the Decree was read by match but deleted before record).
func RecordFire(ctx context.Context, db ExecQueryRower, decree, subject string, firedAt time.Time) error {
	const sql = `
INSERT INTO oracle_fires (decree, subject, fired_at)
VALUES ($1, $2, $3)
ON CONFLICT (decree, subject) DO UPDATE SET fired_at = EXCLUDED.fired_at`
	if _, err := db.Exec(ctx, sql, decree, subject, firedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("oracle: record fire FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("oracle: record fire: %w", err)
	}
	return nil
}

// BumpCircuit atomically increments the per-decree fixed-window fire
// counter in `oracle_circuit` (circuit breaker, ADR-030(a), beacons S4) and
// returns the counter AFTER the increment. window is the window length; now is
// the moment of firing (the same one as cooldown/audit).
//
// Fixed-window semantics: if the current row's window has expired
// (window_start ≤ now - window), it is reset (window_start = now,
// fire_count = 1); otherwise fire_count += 1. The first fire (no row) is an
// INSERT with fire_count = 1.
//
// Cluster-safe: a single INSERT … ON CONFLICT DO UPDATE … RETURNING statement
// under a single row lock serializes read-modify-write — concurrent
// BumpCircuit calls from different Keeper instances on the same Decree don't lose increments.
// An FK violation on a missing Decree is a caller programming error (the Decree
// was read by match but deleted before the bump): a wrapped error.
func BumpCircuit(ctx context.Context, db ExecQueryRower, decree string, now time.Time, window time.Duration) (int, error) {
	const sql = `
INSERT INTO oracle_circuit (decree, window_start, fire_count)
VALUES ($1, $2, 1)
ON CONFLICT (decree) DO UPDATE SET
  window_start = CASE WHEN oracle_circuit.window_start <= $2 - $3::interval THEN $2 ELSE oracle_circuit.window_start END,
  fire_count   = CASE WHEN oracle_circuit.window_start <= $2 - $3::interval THEN 1 ELSE oracle_circuit.fire_count + 1 END
RETURNING fire_count`
	var fireCount int
	err := db.QueryRow(ctx, sql, decree, now, pgutil.Interval(window)).Scan(&fireCount)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return 0, fmt.Errorf("oracle: bump circuit FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return 0, fmt.Errorf("oracle: bump circuit: %w", err)
	}
	return fireCount, nil
}

// TripDecree auto-disables a Decree via the circuit breaker: flips enabled
// true→false (ADR-030(a)). Returns tripped=true if this operation actually
// disabled the rule (RowsAffected==1) — single-winner: on a concurrent trip
// from multiple Keeper instances exactly one wins (`WHERE enabled=true`
// serializes via row lock), the rest get RowsAffected==0 and do NOT duplicate
// alert/audit/metric. now is written to updated_at (a Decree mutation).
func TripDecree(ctx context.Context, db ExecQueryRower, decree string, now time.Time) (bool, error) {
	const sql = `UPDATE decrees SET enabled = false, updated_at = $2 WHERE name = $1 AND enabled = true`
	tag, err := db.Exec(ctx, sql, decree, now)
	if err != nil {
		return false, fmt.Errorf("oracle: trip decree: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
