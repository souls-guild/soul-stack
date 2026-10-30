// Package maintenancetest is the shared fixture of the guard that every reader of
// maintenance windows applies the cluster default (NIM-1138): the same slots seen
// three ways, and a database fake answering the reads of [maintenance.LoadRules].
package maintenancetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
)

// Incarnation is the one incarnation the fixture's windows belong to.
const Incarnation = "redis-prod"

// Slot is a weekly window, every day, that opens three hours after now (UTC, to
// the minute) and lasts an hour, and that opening: closed at now, so a reader
// that applies it answers differently from one that does not.
func Slot(now time.Time) (maintenance.Window, time.Time) {
	start := now.UTC().Add(3 * time.Hour).Truncate(time.Minute)
	return maintenance.Window{
		ID: "11111111-1111-4111-8111-000000000001", IncarnationID: Incarnation, Kind: maintenance.KindWeekly,
		Days:      []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		StartTime: start.Format("15:04"), DurationMinutes: 60, Timezone: "UTC", Comment: "nightly",
	}, start
}

// Mode is one way a reader sees the slots: as the incarnation's own windows
// (Own), as the cluster default (Defaults), or not at all.
type Mode struct {
	Name     string
	Own      []maintenance.Window
	Defaults maintenance.DefaultsSource
}

// Mode names.
const (
	ByDefault = "default"
	AsOwn     = "own"
	Nowhere   = "none"
)

// Modes returns the three modes over slots. A reader passes the guard when
// ByDefault and AsOwn give one answer and Nowhere another.
func Modes(slots ...maintenance.Window) []Mode {
	defaults := make([]maintenance.Window, len(slots))
	for i, w := range slots {
		w.ID, w.IncarnationID = fmt.Sprintf("maintenance.default_windows[%d]", i), ""
		defaults[i] = w
	}
	return []Mode{
		{Name: ByDefault, Defaults: func() []maintenance.Window { return slices.Clone(defaults) }},
		{Name: AsOwn, Own: slices.Clone(slots)},
		{Name: Nowhere},
	}
}

// Agree fails t unless reader answered alike ByDefault and AsOwn — the default
// applied as the incarnation's own windows are — and otherwise Nowhere, which
// shows the slots decide the answer at all. answers is keyed by mode name.
func Agree(t testing.TB, reader string, answers map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(answers[ByDefault], answers[AsOwn]) {
		t.Errorf("%s: under the cluster default %+v, with the same slots as its own windows %+v: the reader does not apply the default",
			reader, answers[ByDefault], answers[AsOwn])
	}
	if reflect.DeepEqual(answers[ByDefault], answers[Nowhere]) {
		t.Errorf("%s: %+v with the slots and without them: this case proves nothing about the default", reader, answers[Nowhere])
	}
}

// DB answers the two reads of [maintenance.LoadRules] from Windows and Freezes,
// for the incarnations the query names. Every other statement is an error, so a
// reader that reads something else is not answered by accident. It satisfies
// maintenance.DB and the wider voyage surfaces (CopyFrom, BeginTx).
type DB struct {
	Windows []maintenance.Window
	Freezes []maintenance.Freeze
}

func (d *DB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	ids := idsOf(args)
	var rows [][]any
	switch {
	case strings.Contains(sql, "FROM incarnation_maintenance_windows"):
		for _, w := range d.Windows {
			if !slices.Contains(ids, w.IncarnationID) {
				continue
			}
			var starts, ends *time.Time
			if w.Kind == maintenance.KindOnce {
				s, e := w.StartsAt, w.EndsAt
				starts, ends = &s, &e
			}
			kind := w.Kind
			if kind == "" {
				kind = maintenance.KindWeekly
			}
			rows = append(rows, []any{w.ID, w.IncarnationID, kind, w.Days, w.StartTime, w.DurationMinutes,
				w.Timezone, starts, ends, w.Comment, w.CreatedAt, w.UpdatedAt})
		}
	case strings.Contains(sql, "FROM incarnation_freezes"):
		for _, f := range d.Freezes {
			if slices.Contains(ids, f.IncarnationID) {
				rows = append(rows, []any{f.ID, f.IncarnationID, f.StartsAt, f.EndsAt, f.Comment, f.CreatedAt, f.UpdatedAt})
			}
		}
	default:
		return nil, fmt.Errorf("maintenancetest: unexpected query %q", sql)
	}
	return &Rows{rows: rows}, nil
}

func (d *DB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("maintenancetest: unexpected exec %q", sql)
}

func (d *DB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return errRow{fmt.Errorf("maintenancetest: unexpected query %q", sql)}
}

func (d *DB) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("maintenancetest: unexpected copy")
}

func (d *DB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("maintenancetest: unexpected transaction")
}

// idsOf reads the incarnation ids out of a query's first argument: the
// `= ANY($1::text[])` of LoadRules, or one id.
func idsOf(args []any) []string {
	if len(args) == 0 {
		return nil
	}
	switch v := args[0].(type) {
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}

// Rows is pgx.Rows over positional values, assigned by reflection.
type Rows struct {
	rows [][]any
	i    int
}

func (r *Rows) Close()                                       {}
func (r *Rows) Err() error                                   { return nil }
func (r *Rows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *Rows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *Rows) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *Rows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *Rows) RawValues() [][]byte                          { return nil }
func (r *Rows) Conn() *pgx.Conn                              { return nil }
func (r *Rows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("maintenancetest: %d destinations for %d columns", len(dest), len(row))
	}
	for i, v := range row {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(v))
	}
	return nil
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
