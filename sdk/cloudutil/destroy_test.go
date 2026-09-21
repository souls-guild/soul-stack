package cloudutil

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fastCfg is a zero-delay budget for the destroy tests — they assert on call
// counts and diagnostics, not on real timing.
func fastCfg(attempts int) BackoffConfig {
	return BackoffConfig{Initial: time.Millisecond, Max: time.Millisecond, Factor: 1, MaxAttempts: attempts}
}

// deleteRecorder counts delete calls per VM so the tests can tell a single
// issue from a re-issue.
type deleteRecorder struct {
	calls map[string]int
	err   error
}

func newDeleteRecorder() *deleteRecorder { return &deleteRecorder{calls: map[string]int{}} }

func (d *deleteRecorder) del(_ context.Context, vmID string) error {
	d.calls[vmID]++
	return d.err
}

// TestConfirmDestroy_GoneOnFirstProbe: the common case must stay cheap — one
// delete, one confirming poll, no extra rounds.
func TestConfirmDestroy_GoneOnFirstProbe(t *testing.T) {
	rec := newDeleteRecorder()
	polls := 0
	probe := func(context.Context, string) GoneResult {
		polls++
		return GoneResult{Gone: true}
	}
	res, err := ConfirmDestroy(context.Background(), fastCfg(10), []string{"vm-1"}, rec.del, probe, nil)
	if err != nil {
		t.Fatalf("ConfirmDestroy: %v", err)
	}
	if len(res) != 1 || !res[0].Gone {
		t.Fatalf("res=%+v, want one gone VM", res)
	}
	if rec.calls["vm-1"] != 1 {
		t.Errorf("delete called %d times, want exactly 1", rec.calls["vm-1"])
	}
	if polls != 1 {
		t.Errorf("polls=%d, want 1 (the happy path must not add rounds)", polls)
	}
}

// TestConfirmDestroy_NotConfirmed_IsNotSuccess is the core of NIM-191: the
// provider accepted the delete but the VM is still there — that must NOT be
// reported as a successful destroy.
func TestConfirmDestroy_NotConfirmed_IsNotSuccess(t *testing.T) {
	rec := newDeleteRecorder()
	probe := func(context.Context, string) GoneResult {
		return GoneResult{State: "DELETING"} // accepted, never actually disappears
	}
	res, err := ConfirmDestroy(context.Background(), fastCfg(3), []string{"vm-stay"}, rec.del, probe, nil)
	if !errors.Is(err, ErrDestroyDeadline) {
		t.Fatalf("err=%v, want ErrDestroyDeadline", err)
	}
	if len(res) != 1 || res[0].Gone {
		t.Fatalf("res=%+v, want the VM reported as NOT gone", res)
	}
	if res[0].VMID != "vm-stay" {
		t.Errorf("VMID=%q, want vm-stay (the caller needs it to retry/alert)", res[0].VMID)
	}
}

// TestConfirmDestroy_ReissuesOnDeleteFailed: a VM the provider refused to
// delete (parking it in DELETE_FAILED) never leaves that state on its own —
// the delete has to be issued again, which is what the manual recovery did.
func TestConfirmDestroy_ReissuesOnDeleteFailed(t *testing.T) {
	rec := newDeleteRecorder()
	polls := 0
	probe := func(context.Context, string) GoneResult {
		polls++
		if polls < 3 {
			return GoneResult{State: "DELETE_FAILED", DeleteFailed: true}
		}
		return GoneResult{Gone: true}
	}
	res, err := ConfirmDestroy(context.Background(), fastCfg(10), []string{"vm-df"}, rec.del, probe, nil)
	if err != nil {
		t.Fatalf("ConfirmDestroy: %v", err)
	}
	if !res[0].Gone {
		t.Fatalf("res=%+v, want gone after the re-issued delete", res[0])
	}
	if rec.calls["vm-df"] != 3 {
		t.Errorf("delete called %d times, want 3 (initial + one per DELETE_FAILED observation)", rec.calls["vm-df"])
	}
}

// TestConfirmDestroy_NoReissueWhileDeleting: a delete that is merely still in
// flight must not be spammed — only an explicit provider-side failure warrants
// a re-issue.
func TestConfirmDestroy_NoReissueWhileDeleting(t *testing.T) {
	rec := newDeleteRecorder()
	polls := 0
	probe := func(context.Context, string) GoneResult {
		polls++
		if polls < 3 {
			return GoneResult{State: "DELETING"}
		}
		return GoneResult{Gone: true}
	}
	if _, err := ConfirmDestroy(context.Background(), fastCfg(10), []string{"vm-slow"}, rec.del, probe, nil); err != nil {
		t.Fatalf("ConfirmDestroy: %v", err)
	}
	if rec.calls["vm-slow"] != 1 {
		t.Errorf("delete called %d times, want 1 (an in-flight delete must not be re-issued)", rec.calls["vm-slow"])
	}
}

// TestConfirmDestroy_DeadlineDiagnostics_DeleteRejected: when the budget runs
// out on a VM the provider kept refusing to delete, the message must say so —
// a bigger budget is not the fix there.
func TestConfirmDestroy_DeadlineDiagnostics_DeleteRejected(t *testing.T) {
	rec := newDeleteRecorder()
	probe := func(_ context.Context, vmID string) GoneResult {
		if vmID == "vm-ok" {
			return GoneResult{Gone: true}
		}
		return GoneResult{State: "DELETE_FAILED", DeleteFailed: true}
	}
	_, err := ConfirmDestroy(context.Background(), fastCfg(3), []string{"vm-ok", "vm-df"}, rec.del, probe, nil)
	var de *DestroyDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%v (%T), want *DestroyDeadlineError", err, err)
	}
	if de.Gone != 1 || de.Total != 2 {
		t.Errorf("gone=%d total=%d, want 1/2", de.Gone, de.Total)
	}
	if len(de.Pending) != 1 || de.Pending[0].VMID != "vm-df" || !de.Pending[0].DeleteRejected {
		t.Fatalf("pending=%+v, want only vm-df flagged as delete-rejected", de.Pending)
	}
	msg := err.Error()
	if !strings.Contains(msg, "vm-df") || !strings.Contains(msg, "DELETE_FAILED") {
		t.Errorf("message=%q, want the VM and its last state named", msg)
	}
	if !strings.Contains(msg, "still present") {
		t.Errorf("message=%q, want it to state the VMs are still present", msg)
	}
	if strings.Contains(msg, WaitBudgetEnv) {
		t.Errorf("message=%q, must not suggest a bigger budget when the provider rejects the delete", msg)
	}
}

// TestConfirmDestroy_DeadlineDiagnostics_StillDeleting: the opposite case — the
// teardown was progressing, so the budget really is the knob.
func TestConfirmDestroy_DeadlineDiagnostics_StillDeleting(t *testing.T) {
	rec := newDeleteRecorder()
	probe := func(context.Context, string) GoneResult { return GoneResult{State: "DELETING"} }
	_, err := ConfirmDestroy(context.Background(), fastCfg(3), []string{"vm-slow"}, rec.del, probe, nil)
	var de *DestroyDeadlineError
	if !errors.As(err, &de) {
		t.Fatalf("err=%v, want *DestroyDeadlineError", err)
	}
	if de.Pending[0].DeleteRejected {
		t.Errorf("pending=%+v, want no delete-rejected flag", de.Pending[0])
	}
	if msg := err.Error(); !strings.Contains(msg, WaitBudgetEnv) {
		t.Errorf("message=%q, want the budget knob mentioned for a teardown still in flight", msg)
	}
}

// TestConfirmDestroy_TerminalProbeErr: a deterministic probe failure closes
// that VM instead of spinning until the budget dies.
func TestConfirmDestroy_TerminalProbeErr(t *testing.T) {
	rec := newDeleteRecorder()
	boom := errors.New("permission denied reading vm")
	probe := func(context.Context, string) GoneResult { return GoneResult{Err: boom} }
	res, err := ConfirmDestroy(context.Background(), fastCfg(10), []string{"vm-err"}, rec.del, probe, nil)
	if err != nil {
		t.Fatalf("ConfirmDestroy returned %v; per-VM errors belong in the results", err)
	}
	if len(res) != 1 || res[0].Gone || !errors.Is(res[0].Err, boom) {
		t.Fatalf("res=%+v, want the terminal probe error on the VM", res)
	}
}

// TestConfirmDestroy_DeleteErrorSurfacesWhenNotGone: if the delete call itself
// failed and the VM never disappeared, the caller must see why.
func TestConfirmDestroy_DeleteErrorSurfacesWhenNotGone(t *testing.T) {
	rec := newDeleteRecorder()
	rec.err = errors.New("quota service unavailable")
	probe := func(context.Context, string) GoneResult { return GoneResult{State: "RUNNING"} }
	res, err := ConfirmDestroy(context.Background(), fastCfg(2), []string{"vm-x"}, rec.del, probe, nil)
	if !errors.Is(err, ErrDestroyDeadline) {
		t.Fatalf("err=%v, want ErrDestroyDeadline", err)
	}
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "quota service unavailable") {
		t.Errorf("res=%+v, want the delete error attached to the VM", res[0])
	}
}

// TestConfirmDestroy_DeleteErrorIgnoredWhenGone: a delete that errored but
// still took effect (or raced another destroy) is a success, not a failure.
func TestConfirmDestroy_DeleteErrorIgnoredWhenGone(t *testing.T) {
	rec := newDeleteRecorder()
	rec.err = errors.New("gateway timeout")
	probe := func(context.Context, string) GoneResult { return GoneResult{Gone: true} }
	res, err := ConfirmDestroy(context.Background(), fastCfg(5), []string{"vm-y"}, rec.del, probe, nil)
	if err != nil {
		t.Fatalf("ConfirmDestroy: %v", err)
	}
	if !res[0].Gone || res[0].Err != nil {
		t.Errorf("res=%+v, want a clean success once the VM is confirmed gone", res[0])
	}
}

// TestConfirmDestroy_CtxCancel_KeepsVMIDs mirrors the anti-orphan contract of
// WaitUntilReady: a cancelled teardown still tells the caller which VMs were
// not confirmed gone.
func TestConfirmDestroy_CtxCancel_KeepsVMIDs(t *testing.T) {
	rec := newDeleteRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second, Factor: 2, MaxAttempts: 50}
	probe := func(context.Context, string) GoneResult { return GoneResult{State: "DELETING"} }
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	res, err := ConfirmDestroy(ctx, cfg, []string{"vm-1", "vm-2"}, rec.del, probe, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if len(res) != 2 {
		t.Fatalf("res len=%d, want both VMs reported", len(res))
	}
	for _, r := range res {
		if r.VMID == "" {
			t.Error("anti-orphan: every result must carry its vm_id")
		}
	}
}

// TestConfirmDestroy_ProgressReportsCounts keeps teardown visible in the event
// stream instead of a silent multi-minute stall.
func TestConfirmDestroy_ProgressReportsCounts(t *testing.T) {
	rec := newDeleteRecorder()
	var msgs []string
	probe := func(context.Context, string) GoneResult { return GoneResult{State: "DELETING"} }
	_, _ = ConfirmDestroy(context.Background(), fastCfg(3), []string{"vm-1", "vm-2"},
		rec.del, probe, func(m string) { msgs = append(msgs, m) })
	if len(msgs) == 0 {
		t.Fatal("no progress messages")
	}
	if !strings.Contains(msgs[0], "0/2 gone") {
		t.Errorf("progress=%q, want a gone counter", msgs[0])
	}
}

// TestConfirmDestroy_EmptyInput: destroying nothing is a no-op success, not a
// poll round.
func TestConfirmDestroy_EmptyInput(t *testing.T) {
	rec := newDeleteRecorder()
	probe := func(context.Context, string) GoneResult {
		t.Fatal("probe must not be called for an empty id list")
		return GoneResult{}
	}
	res, err := ConfirmDestroy(context.Background(), fastCfg(3), nil, rec.del, probe, nil)
	if err != nil || len(res) != 0 {
		t.Fatalf("res=%+v err=%v, want an empty success", res, err)
	}
}
