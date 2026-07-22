//go:build e2e_live

// FC-4 L3b: failed_when:false = ignore_errors on a REAL module failure during
// real-apply. L3a-stub doesn't evaluate failed_when -> the best-effort semantics
// (ignore_errors) and mapping a real failure into register.<name>.ignored_error
// are NOT proven there. This test runs a GENUINE soul binary in a Debian-12 container.
//
// Service tests/e2e-live/fc4-ignore-errors-live (NOT examples/** - WIP zone):
// core.exec.run of a nonexistent binary REALLY fails on Soul (fork/exec: file
// not found -> util.SendFailed -> last.failed=true). Two mirror scenarios:
//   - create:    failing task + failed_when:false -> run SUCCESS, the original
//     error goes into register.fail_probe.ignored_error (NOT lost).
//   - fail_hard: THE SAME task WITHOUT failed_when -> run FAILED -> error_locked.
//
// ASSERT (* ignore_errors proof on real-apply):
//  1. create: apply_runs success on the host (failed_when:false suppressed the failure).
//  2. * register.fail_probe.ignored_error persists with the REAL module error
//     (non-empty + contains the exec-failure signature). Field name checked against
//     soul/internal/runtime/applyrunner.go (ev.RegisterData["ignored_error"],
//     ignore_errors audit, ~:959). register.failed == false (failure suppressed).
//  3. * Contrast: fail_hard (same task without failed_when) -> run FAILED,
//     incarnation error_locked. Proves success in (1) is due specifically to
//     failed_when:false, not "the module doesn't fail".
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bFC4IgnoreErrorsLive_SuppressesRealModuleFailure(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/fc4-ignore-errors-live",
		ServiceName: "fc4-ignore-errors-live",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	const sid = "soul-live-a.example.com"
	if got := stack.SoulContainers[0].SID; got != sid {
		t.Fatalf("SoulContainers[0].SID = %q, expected %q", got, sid)
	}

	const incName = "test-fc4-ignore-errors"

	// Membership BEFORE Create: the roster resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124). Without it the scenario sees no_hosts -> zero apply_runs rows.
	stack.AddMember(t, 0, incName)

	// ── (1) create: real module failure + failed_when:false -> SUCCESS ─────
	// POST /v1/incarnations auto-runs the create scenario. On the single host, the
	// core.exec.run task fails (binary not found), failed_when "false" overrides the
	// failure -> task OK -> run succeeds.
	inc, createApplyID := stack.CreateIncarnationWithApply(t, incName, "fc4-ignore-errors-live@main", nil)

	// 120 s with margin for container cold-start (the task itself is instant - exec
	// fails right at process start, no apt/network involved).
	stack.WaitApplySuccess(t, createApplyID, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// * apply_runs host status = success: failed_when:false suppressed the real failure.
	stack.AssertApplyHostStatus(t, createApplyID, sid, "success")

	// ── (2) * register.fail_probe.ignored_error carries the REAL error ────────
	// Field name is ignored_error (applyrunner.go ignore_errors audit): when
	// !failed && moduleErr != nil the original error is put into register_data.
	// The single task in the plan -> plan_index=0. Exact value is the text of the
	// exec failure from the Go runtime (brittle for ==), so we check non-empty +
	// signature.
	const planIdx = 0
	assertRegisterFieldContains(t, stack, createApplyID, sid, planIdx, "ignored_error",
		"/nonexistent/fc4-deliberate-fail")

	// failed=false in register - the failure is suppressed, the final flow-control outcome is OK.
	// This is an exact value, safe for ==.
	stack.AssertTaskRegisterField(t, createApplyID, sid, planIdx, "failed", "false")

	// ── (3) * Contrast: fail_hard (same task without failed_when) -> FAILED ────
	// The incarnation is ready after create. Run fail_hard - the same failing
	// task, but without failed_when:false. The real failure is NOT suppressed -> run
	// FAILED -> incarnation error_locked. Without this contrast, success in (1) could
	// have been chalked up to "the module doesn't fail" - but it does (proven here).
	failApplyID := stack.RunScenario(t, inc, "fail_hard", nil)

	// fail_hard leaves the incarnation in error_locked (run.go §7: state_changes aren't
	// committed on the terminal-failed barrier). WaitIncarnationStatus fails if
	// any OTHER terminal is reached (including ready - which would be an
	// ignore_errors regression: the failure must NOT be suppressed without the flag).
	stack.WaitIncarnationStatus(t, inc, "error_locked", 120)

	// * apply_runs host status for fail_hard = failed: same module, same error, but without
	// failed_when:false the run fails. The contrast with (1) proves the suppression comes
	// specifically from failed_when, not from the module itself.
	stack.AssertApplyHostStatus(t, failApplyID, sid, "failed")
}

// assertRegisterFieldContains - register_data->>field for a host (plan_index)
// must be non-empty and contain the want substring. AssertTaskRegisterField does an exact ==,
// but ignored_error is the text of the Go runtime exec failure (depends on version/OS),
// so it needs a "non-empty + signature" check, not an exact match.
// Proves the real stderr/diagnostic of the failure actually made it into
// register, not an empty stub.
func assertRegisterFieldContains(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx int, field, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got string
	err := stack.DB().QueryRow(ctx, `
		SELECT COALESCE(register_data->>$4, '<null>')
		FROM apply_task_register
		WHERE apply_id = $1 AND sid = $2 AND plan_index = $3
	`, applyID, sid, planIdx, field).Scan(&got)
	if err != nil {
		t.Fatalf("assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): no register row (task without register:/real soul didn't return register?): %v",
			applyID, sid, planIdx, field, err)
	}
	if got == "" || got == "<null>" {
		t.Fatalf("* assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): field is EMPTY (%q) - ignored_error was not persisted / the real failure never made it into register",
			applyID, sid, planIdx, field, got)
	}
	if !strings.Contains(got, want) {
		t.Fatalf("* assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): %q does NOT contain %q - register carries the wrong error",
			applyID, sid, planIdx, field, got, want)
	}
}
