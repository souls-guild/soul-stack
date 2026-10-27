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

// statusRowsDB answers SelectRunStatus with one row per status, each carrying
// whether the run holds its incarnation's applying lock.
type statusRowsDB struct {
	statuses []applyrun.Status
	lockHeld bool
	dropped  int
}

func (d *statusRowsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (d *statusRowsDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (d *statusRowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &statusRows{statuses: d.statuses, lockHeld: d.lockHeld, dropped: d.dropped, i: -1}, nil
}

type statusRows struct {
	statuses []applyrun.Status
	lockHeld bool
	// dropped — how many of the leading rows are a dropped host's.
	dropped int
	i       int
}

func (r *statusRows) Close()                                       {}
func (r *statusRows) Err() error                                   { return nil }
func (r *statusRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *statusRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *statusRows) Next() bool                                   { r.i++; return r.i < len(r.statuses) }
func (r *statusRows) Values() ([]any, error)                       { return nil, nil }
func (r *statusRows) RawValues() [][]byte                          { return nil }
func (r *statusRows) Conn() *pgx.Conn                              { return nil }

// Scan answers SelectRunStatus's (status, lock_held, dropped, handled) and, for any
// other shape, fills the leading (sid, status) of SelectStatusesByApplyID: an awaiter
// that went back to reading the rows alone fails on its verdict, not on a scan.
func (r *statusRows) Scan(dest ...any) error {
	if len(dest) == 4 {
		*dest[0].(*string) = string(r.statuses[r.i])
		*dest[1].(*bool) = r.lockHeld
		*dest[2].(*bool) = r.i < r.dropped
		*dest[3].(*bool) = false
		return nil
	}
	*dest[0].(*string) = "host-" + string(rune('a'+r.i))
	*dest[1].(*string) = string(r.statuses[r.i])
	return nil
}

// TestVoyageAwaiterPollOutcome — the production awaiter judges an incarnation's
// run by the status the API shows (SelectRunStatus → OutcomeOfRun), so a run
// status the fold learns reaches the Voyage without a second copy of the fold here.
// The verdicts are the ones its own fold gave before, plus a run between two
// steps, every row terminal and its lock still held, which is not done (NIM-972).
func TestVoyageAwaiterPollOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     []applyrun.Status
		lockHeld bool
		dropped  int
		outcome  voyageorch.TargetOutcome
		done     bool
	}{
		{"no rows yet", nil, false, 0, "", false},
		{"still dispatched", []applyrun.Status{applyrun.StatusFailed, applyrun.StatusDispatched}, false, 0, "", false},
		{"success + no_match", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusNoMatch}, false, 0, voyageorch.OutcomeSucceeded, true},
		{"orphaned host", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusOrphaned}, false, 0, voyageorch.OutcomeFailed, true},
		{"failed beats cancelled", []applyrun.Status{applyrun.StatusCancelled, applyrun.StatusFailed}, false, 0, voyageorch.OutcomeFailed, true},
		{"cancelled", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusCancelled}, false, 0, voyageorch.OutcomeCancelled, true},
		{"between steps", []applyrun.Status{applyrun.StatusSuccess, applyrun.StatusSuccess}, true, 0, "", false},
		// NIM-948: a run that finished without a dropped host reads partial_failed,
		// which a Voyage counts as failed.
		{"partial_failed", []applyrun.Status{applyrun.StatusFailed, applyrun.StatusSuccess}, false, 1, voyageorch.OutcomeFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &voyagePgIncarnationAwaiter{
				db:     &statusRowsDB{statuses: tc.rows, lockHeld: tc.lockHeld, dropped: tc.dropped},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			o, done := a.pollOutcome(context.Background(), "01HX0000000000000000000000")
			if o != tc.outcome || done != tc.done {
				t.Errorf("pollOutcome(%v) = (%q, %v), want (%q, %v)", tc.rows, o, done, tc.outcome, tc.done)
			}
		})
	}
}
