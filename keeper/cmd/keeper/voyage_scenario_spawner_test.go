package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/keeper/internal/voyageorch"
)

// TestScenarioStatusRefusal pins which incarnation statuses a Voyage spawn refuses
// before Start, and what it records. The set mirrors lockRun: ready and drift run.
// An error_locked refusal must name the run that locked it — that sentence is what
// the operator reads on the target row instead of a bare `failed` (NIM-989).
func TestScenarioStatusRefusal(t *testing.T) {
	t.Parallel()
	applying := "01HAPPLYING"
	cases := []struct {
		status   incarnation.Status
		details  map[string]any
		applying *string
		code     string
		inMsg    []string
	}{
		{status: incarnation.StatusReady},
		{status: incarnation.StatusDrift},
		{
			status:  incarnation.StatusErrorLocked,
			details: map[string]any{"apply_id": "01HLOCKER", "reason": "apply_failed"},
			code:    voyage.TargetErrorIncarnationErrorLocked,
			inMsg:   []string{"error_locked by run 01HLOCKER", "/v1/incarnations/inc/unlock", "/v1/incarnations/inc/rerun-last"},
		},
		{
			status:   incarnation.StatusApplying,
			applying: &applying,
			code:     voyage.TargetErrorIncarnationApplying,
			inMsg:    []string{"applying run 01HAPPLYING"},
		},
		{status: incarnation.StatusMigrationFailed, code: voyage.TargetErrorIncarnationNotRunnable, inMsg: []string{"migration_failed"}},
		{status: incarnation.StatusDestroying, code: voyage.TargetErrorIncarnationNotRunnable, inMsg: []string{"destroying"}},
		{status: incarnation.StatusDestroyFailed, code: voyage.TargetErrorIncarnationNotRunnable, inMsg: []string{"destroy_failed"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			inc := &incarnation.Incarnation{ID: "inc", Status: tc.status, StatusDetails: tc.details, ApplyingApplyID: tc.applying}
			got := scenarioStatusRefusal(inc)
			if tc.code == "" {
				if got != nil {
					t.Fatalf("status %s refused with %+v, want it left to Start", tc.status, got.Reason)
				}
				return
			}
			if got == nil {
				t.Fatalf("status %s not refused, want %s", tc.status, tc.code)
			}
			if got.Reason.Code != tc.code || !voyage.IsTargetErrorCode(got.Reason.Code) {
				t.Errorf("code = %q, want documented %q", got.Reason.Code, tc.code)
			}
			for _, want := range tc.inMsg {
				if !strings.Contains(got.Reason.Message, want) {
					t.Errorf("message %q does not contain %q", got.Reason.Message, want)
				}
			}
			var refusal *voyageorch.SpawnRefusal
			if !errors.As(error(got), &refusal) || got.Err == nil {
				t.Errorf("refusal %v carries no cause for the log", got)
			}
		})
	}
}

// Only ready and drift reach Start; every other status is refused with a reason,
// including one the switch does not name, which the default branch refuses as
// not runnable (fail-closed, parity lockRun).
func TestScenarioStatusRefusal_OnlyReadyAndDriftRun(t *testing.T) {
	t.Parallel()
	for _, s := range []incarnation.Status{
		incarnation.StatusReady, incarnation.StatusApplying, incarnation.StatusErrorLocked,
		incarnation.StatusMigrationFailed, incarnation.StatusDestroying, incarnation.StatusDestroyFailed,
		incarnation.StatusDrift, incarnation.Status("added_later"),
	} {
		runs := s == incarnation.StatusReady || s == incarnation.StatusDrift
		if got := scenarioStatusRefusal(&incarnation.Incarnation{ID: "inc", Status: s}); (got == nil) != runs {
			t.Errorf("status %q: refused = %v, want refused = %v", s, got != nil, !runs)
		}
	}
}

// A status that changed between the spawner's own check and lockRun still yields a
// reason, from the sentinel Start returns; anything else is left to spawn_error.
func TestStartRefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		code string
	}{
		{fmt.Errorf("start: %w", scenario.ErrLocked), voyage.TargetErrorIncarnationErrorLocked},
		{fmt.Errorf("start: %w", scenario.ErrAlreadyRunning), voyage.TargetErrorIncarnationApplying},
		{fmt.Errorf("start: %w: destroying", scenario.ErrNotRunnable), voyage.TargetErrorIncarnationNotRunnable},
		{fmt.Errorf("start: %w", scenario.ErrShuttingDown), ""},
	}
	for _, tc := range cases {
		got := startRefusal("inc", tc.err)
		var refusal *voyageorch.SpawnRefusal
		isRefusal := errors.As(got, &refusal)
		if tc.code == "" {
			if isRefusal {
				t.Errorf("%v: refused with %q, want a plain error", tc.err, refusal.Reason.Code)
			}
			continue
		}
		if !isRefusal || refusal.Reason.Code != tc.code || refusal.Reason.Message == "" {
			t.Errorf("%v: got %v, want a refusal with code %q and a message", tc.err, got, tc.code)
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%v: the cause is lost from the chain", tc.err)
		}
	}
}

// incRow answers incarnation.SelectByID with one row in scanIncarnation's column
// order; err answers it with that error instead.
type incRow struct {
	status  string
	details []byte
	err     error
}

func (r incRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	now := time.Now()
	values := []any{"inc", "redis", "v1", 1, []byte("{}"), r.status, r.details, nil,
		now, now, []string(nil), []byte("{}"), "create", nil, nil}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = values[i].(string)
		case *int:
			*d = values[i].(int)
		case *time.Time:
			*d = values[i].(time.Time)
		case *[]byte:
			if values[i] != nil {
				*d = values[i].([]byte)
			}
		case *[]string:
			*d = nil
		case **string:
			if v, ok := values[i].(string); ok {
				*d = &v
			} else {
				*d = nil
			}
		}
	}
	return nil
}

type incReader struct{ row incRow }

func (r incReader) QueryRow(context.Context, string, ...any) pgx.Row { return r.row }
func (incReader) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("incReader: unexpected Exec")
}
func (incReader) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("incReader: unexpected Query")
}

type anyServiceResolver struct{}

func (anyServiceResolver) Resolve(string) (artifact.ServiceRef, bool) {
	return artifact.ServiceRef{}, true
}

type startAnswer struct{ err error }

func (s startAnswer) Start(context.Context, scenario.RunSpec) error { return s.err }

// The spawner itself turns every refusal into a reason: one it sees in the row
// before Start names the blocking run, one Start reports after a lost race
// still carries its code. Without that the target reads spawn_error — or a bare
// failed — again.
func TestVoyageScenarioSpawner_RefusalsCarryReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		row   incRow
		start error
		code  string
		inMsg string
	}{
		{"locked in the row", incRow{status: "error_locked", details: []byte(`{"apply_id":"01HLOCKER"}`)}, nil,
			voyage.TargetErrorIncarnationErrorLocked, "by run 01HLOCKER"},
		{"locked by Start", incRow{status: "ready"}, fmt.Errorf("lock: %w", scenario.ErrLocked),
			voyage.TargetErrorIncarnationErrorLocked, "is error_locked"},
		{"applying by Start", incRow{status: "ready"}, scenario.ErrAlreadyRunning,
			voyage.TargetErrorIncarnationApplying, "already applying"},
		{"not found", incRow{err: pgx.ErrNoRows}, nil,
			voyage.TargetErrorIncarnationNotFound, "no longer exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := &voyageScenarioSpawner{runner: startAnswer{err: tc.start}, reader: incReader{row: tc.row}, resolver: anyServiceResolver{}}
			_, err := sp.SpawnScenarioRun(context.Background(), "V1", "inc", "converge", nil, "archon-alice", nil)
			var refusal *voyageorch.SpawnRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a refusal with code %q", err, tc.code)
			}
			if refusal.Reason.Code != tc.code || !strings.Contains(refusal.Reason.Message, tc.inMsg) {
				t.Errorf("reason = %+v, want code %q and a message containing %q", refusal.Reason, tc.code, tc.inMsg)
			}
		})
	}
}
