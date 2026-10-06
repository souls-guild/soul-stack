package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/voyageorch"
)

// statusRowsDB answers SelectStatusesByApplyID with one row per status; only the
// sid and status columns are filled, which is all the awaiter reads.
type statusRowsDB struct{ statuses []applyrun.Status }

func (d *statusRowsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (d *statusRowsDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (d *statusRowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &statusRows{statuses: d.statuses, i: -1}, nil
}

type statusRows struct {
	statuses []applyrun.Status
	i        int
}

func (r *statusRows) Close()                                       {}
func (r *statusRows) Err() error                                   { return nil }
func (r *statusRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *statusRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *statusRows) Next() bool                                   { r.i++; return r.i < len(r.statuses) }
func (r *statusRows) Values() ([]any, error)                       { return nil, nil }
func (r *statusRows) RawValues() [][]byte                          { return nil }
func (r *statusRows) Conn() *pgx.Conn                              { return nil }
func (r *statusRows) Scan(dest ...any) error {
	*dest[0].(*string) = "host-" + string(rune('a'+r.i))
	*dest[1].(*string) = string(r.statuses[r.i])
	return nil
}

// TestVoyageAwaiterPollOutcome — the production awaiter judges an incarnation's
// run by the status the API shows (AggregateRunStatus → OutcomeOfRun), so a run
// status the fold learns reaches the Voyage without a second copy of the fold here.
// The verdicts are the ones its own fold gave before.
func TestVoyageAwaiterPollOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []applyrun.Status
		outcome voyageorch.TargetOutcome
		done    bool
	}{
		{"no rows yet", nil, "", false},
		{"still dispatched", []applyrun.Status{applyrun.StatusFailed, applyrun.StatusDispatched}, "", false},
		{"success + no_match", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusNoMatch}, voyageorch.OutcomeSucceeded, true},
		{"orphaned host", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusOrphaned}, voyageorch.OutcomeFailed, true},
		{"failed beats cancelled", []applyrun.Status{applyrun.StatusCancelled, applyrun.StatusFailed}, voyageorch.OutcomeFailed, true},
		{"cancelled", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusCancelled}, voyageorch.OutcomeCancelled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &voyagePgIncarnationAwaiter{
				db:     &statusRowsDB{statuses: tc.rows},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			o, done := a.pollOutcome(context.Background(), "01HX0000000000000000000000")
			if o != tc.outcome || done != tc.done {
				t.Errorf("pollOutcome(%v) = (%q, %v), want (%q, %v)", tc.rows, o, done, tc.outcome, tc.done)
			}
		})
	}
}
