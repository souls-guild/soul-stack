package voyage

import (
	"context"
	"strings"
	"testing"
)

func TestMarkTargetRunning_SQLAndArgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	if err := MarkTargetRunning(ctx, fdb, "v1", TargetKindIncarnation, "inc-a", "ap1", 1); err != nil {
		t.Fatalf("MarkTargetRunning: %v", err)
	}
	if !strings.Contains(fdb.execSQL, "status   = 'running'") {
		t.Errorf("SQL без перевода в running: %.200s", fdb.execSQL)
	}
	if !strings.Contains(fdb.execSQL, "vt.status       = 'awaiting'") {
		t.Errorf("SQL без idempotent guard status='awaiting': %.200s", fdb.execSQL)
	}
	if !strings.Contains(fdb.execSQL, "v.attempt       = $5") {
		t.Errorf("SQL без fencing guard voyages.attempt: %.200s", fdb.execSQL)
	}
	if len(fdb.execArgs) != 5 {
		t.Fatalf("execArgs len = %d, want 5", len(fdb.execArgs))
	}
	if fdb.execArgs[3] != "ap1" {
		t.Errorf("apply_id arg = %v, want ap1", fdb.execArgs[3])
	}
	if fdb.execArgs[4] != 1 {
		t.Errorf("attempt arg = %v, want 1", fdb.execArgs[4])
	}
}

func TestMarkTargetRunning_SID_WritesErrandID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	if err := MarkTargetRunning(ctx, fdb, "v1", TargetKindSID, "soul.example", "er1", 1); err != nil {
		t.Fatalf("MarkTargetRunning(sid): %v", err)
	}
	if !strings.Contains(fdb.execSQL, "errand_id = $4") {
		t.Errorf("SQL не пишет errand_id для kind=sid: %.200s", fdb.execSQL)
	}
	if strings.Contains(fdb.execSQL, "apply_id = $4") {
		t.Errorf("SQL пишет apply_id вместо errand_id для kind=sid: %.200s", fdb.execSQL)
	}
	if len(fdb.execArgs) != 5 || fdb.execArgs[3] != "er1" || fdb.execArgs[4] != 1 {
		t.Errorf("execArgs = %v, want back-link er1 в $4, attempt 1 в $5", fdb.execArgs)
	}
}

func TestMarkTargetRunning_Validation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name      string
		id, tid   string
		kind      TargetKind
		applyID   string
		wantInErr string
	}{
		{"empty voyage", "", "inc", TargetKindIncarnation, "ap", "empty voyage_id"},
		{"bad kind", "v", "inc", TargetKind("x"), "ap", "invalid target_kind"},
		{"empty target", "v", "", TargetKindIncarnation, "ap", "empty target_id"},
		{"empty apply", "v", "inc", TargetKindIncarnation, "", "empty back-link id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MarkTargetRunning(ctx, &fakeDB{}, tc.id, tc.kind, tc.tid, tc.applyID, 1)
			if err == nil || !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantInErr)
			}
		})
	}
}

func TestMarkTargetTerminal_SQLAndArgs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fdb := &fakeDB{}
	if err := MarkTargetTerminal(ctx, fdb, "v1", TargetKindIncarnation, "inc-a", TargetStatusSucceeded); err != nil {
		t.Fatalf("MarkTargetTerminal: %v", err)
	}
	if !strings.Contains(fdb.execSQL, "finished_at = NOW()") {
		t.Errorf("SQL без finished_at: %.200s", fdb.execSQL)
	}
	if !strings.Contains(fdb.execSQL, "status NOT IN ('succeeded', 'failed', 'cancelled', 'no_match')") {
		t.Errorf("SQL без idempotent terminal guard: %.200s", fdb.execSQL)
	}
	if got := fdb.execArgs[3]; got != string(TargetStatusSucceeded) {
		t.Errorf("status arg = %v, want succeeded", got)
	}
}

func TestMarkTargetTerminal_RejectsNonTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, st := range []TargetStatus{TargetStatusAwaiting, TargetStatusRunning} {
		err := MarkTargetTerminal(ctx, &fakeDB{}, "v", TargetKindIncarnation, "inc", st)
		if err == nil || !strings.Contains(err.Error(), "not terminal") {
			t.Errorf("status %q: err = %v, want not-terminal", st, err)
		}
	}
}

func TestMarkTargetTerminal_AllTerminalStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, st := range []TargetStatus{TargetStatusSucceeded, TargetStatusFailed, TargetStatusCancelled, TargetStatusNoMatch} {
		if err := MarkTargetTerminal(ctx, &fakeDB{}, "v", TargetKindIncarnation, "inc", st); err != nil {
			t.Errorf("status %q rejected: %v", st, err)
		}
	}
}
