//go:build e2e_live

// FC-2 L3b: changed_when + onchanges cascade from a REAL changed status on
// real-apply (idempotency).
//
// GOAL. L3a-stub (tests/e2e) returns Changed:true/false from a fixture - real
// idempotency of core.file.rendered / core.service is NOT proven, and the onchanges
// cascade can falsely trigger/not trigger on a "fake" changed. Here the
// cascade is run through a GENUINE Soul Apply in a Debian-12 container: SHA-256
// content verification (core.file.rendered) and is-active/is-enabled checks
// (core.service) determine register.changed, which drives soul-side
// skipOnChanges (applyrunner.go::skipOnChanges).
//
// Scenario - examples/service/smoke-nginx-live/create (same as L3b-smoke):
//
//	plan_index 0: core.pkg.installed nginx
//	plan_index 1: core.file.rendered  /etc/nginx/sites-available/default
//	              register: nginx_default_conf  <- onchanges source
//	plan_index 2: core.service.running nginx (enabled:true)
//	plan_index 3: core.service.restarted nginx  onchanges:[nginx_default_conf]  <- handler
//
// All 4 tasks in ONE Passage (passage=0): smoke-nginx doesn't use
// where:/when: on register, so keeper doesn't stratify the plan - the onchanges
// source and handler sit in the same Passage and the handler-skip decision is soul-side
// (skipOnChanges), not cross-passage keeper logic. N=1 run -> plan_index ==
// task position in the plan.
//
//   - CORE OF FC-2 - two runs of the SAME create on ONE incarnation, different apply_id:
//     Run 1 (clean container): file changed=true -> handler CHANGED -> nginx
//     actually restarted.
//     Run 2 (same state, repeat): file changed=false (SHA matched) -> handler
//     onchanges does NOT trigger -> TASK_STATUS_SKIPPED.
//
// If run 2 shows file changed=true on an unchanged file OR the handler
// executes again (not SKIPPED) - this is a REAL idempotency defect in
// core.file.rendered / the onchanges cascade, not a test artifact.
//
// Helpers - FC-0 (AssertTaskStatus / AssertTaskRegisterField read audit_log /
// apply_task_register by plan_index) + AssertHostServiceActive. No new helpers are
// added to the shared harness (FC-2-local asserts live in this file).
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// plan_index of the create-scenario smoke-nginx-live tasks (one Passage, N=1 run ->
// plan_index == position). Source of truth - scenario/create/main.yml.
const (
	fc2PkgPlanIdx     = 0 // core.pkg.installed nginx
	fc2FilePlanIdx    = 1 // core.file.rendered (register: nginx_default_conf)
	fc2ServicePlanIdx = 2 // core.service.running nginx
	fc2HandlerPlanIdx = 3 // core.service.restarted (onchanges: [nginx_default_conf])
	fc2Passage        = 0 // smoke-nginx is not stratified - the single Passage
)

func TestFC2OnchangesIdempotency(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx-live",
		ServiceName: "smoke-nginx-live",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	sid := stack.SoulContainers[0].SID
	if sid != wantSID {
		t.Fatalf("SoulContainers[0].SID = %q, expected %q", sid, wantSID)
	}

	const incName = "fc2-onchanges"
	const nginxConfPath = "/etc/nginx/sites-available/default"

	// Membership BEFORE Create: the roster resolves members via incarnation_membership
	// (ADR-008 amendment/NIM-124). Without it the scenario sees no_hosts -> zero apply_runs (as in L3b-smoke).
	stack.AddMember(t, 0, incName)

	// ── Run 1: clean container ───────────────────────────────────────────
	// POST /v1/incarnations auto-runs create and returns its apply_id.
	inc, apply1 := stack.CreateIncarnationWithApply(t, incName, "smoke-nginx-live@main", map[string]any{
		"hostname": wantSID,
	})

	// 300 s - apt-get update + install nginx + systemctl on a fresh Debian-12.
	stack.WaitApplySuccess(t, apply1, 300)
	stack.WaitIncarnationReady(t, inc, 30)

	// 1.1 file task is actually changed=true (new config was written to disk).
	stack.AssertTaskRegisterField(t, apply1, sid, fc2FilePlanIdx, "changed", "true")

	// 1.2 handler (core.service.restarted, onchanges) triggered -> CHANGED.
	// restarted is unconditionally changed=true (service.go applyRestarted), and onchanges
	// triggered it because the source was changed.
	stack.AssertTaskStatus(t, apply1, sid, fc2HandlerPlanIdx, fc2Passage, "TASK_STATUS_CHANGED")

	// 1.3 the service is actually active (systemctl is-active inside the container).
	stack.AssertHostServiceActive(t, 0, "nginx")

	// ── Run 2: same state, repeat of the same create ─────────────────────────
	// RunScenario(create) on a ready incarnation passes the lock gate (RunTyped
	// only blocks error_locked, incarnation_typed.go) -> new apply_id.
	// Don't touch the container between runs - config is identical.
	apply2 := stack.RunScenario(t, incName, "create", map[string]any{
		"hostname": wantSID,
	})
	stack.WaitApplySuccess(t, apply2, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// 2.1 * file task NOT changed - SHA-256 matched, perm/owner didn't drift
	// (core.file.rendered: changed = contentChanged||modeChanged||ownerChanged).
	// This is exactly the REAL idempotency of core.file.rendered.
	assertFileNotChanged(t, stack, apply2, sid)

	// 2.2 * handler onchanges -> SKIPPED. Source not changed -> soul-side
	// skipOnChanges=true -> core.service.restarted does NOT execute. Core of FC-2:
	// the cascade does not re-trigger on an unchanged file.
	assertHandlerSkipped(t, stack, apply2, sid)

	// 2.3 sanity: pkg / service.running also don't fail on repeat (idempotency
	// of the whole chain, not just the handler). pkg: repo already latest -> OK; service.
	// running: active+enabled -> OK. Allow OK or CHANGED for both (depends on
	// whether repo/enabled drifted between runs), but NOT FAILED/SKIPPED.
	assertNotFailedNotSkipped(t, stack, apply2, sid, fc2PkgPlanIdx, "core.pkg.installed")
	assertNotFailedNotSkipped(t, stack, apply2, sid, fc2ServicePlanIdx, "core.service.running")

	// 2.4 the service remained active (handler skip didn't break the service - it wasn't
	// supposed to touch it).
	stack.AssertHostServiceActive(t, 0, "nginx")

	t.Logf("FC-2 cascade proven: run1 file.changed=true->handler CHANGED->nginx active; "+
		"run2 file.changed=false->handler SKIPPED (idempotency). apply1=%s apply2=%s",
		apply1, apply2)
}

// assertFileNotChanged - diagnostic wrapper over FC-0 register reading:
// run 2 of the file task MUST be changed=false. If changed=true on an
// unchanged file - that's an idempotency defect in core.file.rendered (spurious
// re-render), and the message should say so directly, not "assert failed".
func assertFileNotChanged(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	got := registerChangedField(t, stack, applyID, sid, fc2FilePlanIdx)
	if got != "false" {
		t.Fatalf("* FC-2 IDEMPOTENCY DEFECT: run 2 core.file.rendered changed=%q, "+
			"expected \"false\" (same content/perm/owner on disk). Either the SHA-256 check "+
			"is not idempotent, or perm/owner drift on every apply -> the onchanges cascade "+
			"will falsely trigger on every run. apply=%s sid=%s plan_index=%d",
			got, applyID, sid, fc2FilePlanIdx)
	}
}

// assertHandlerSkipped - core of FC-2: the handler (onchanges on an unchanged source)
// MUST be TASK_STATUS_SKIPPED on run 2. If it's CHANGED/OK - onchanges
// triggered on a non-changed source -> cascade defect (handler executed when it
// shouldn't have; nginx would restart on every no-op apply).
func assertHandlerSkipped(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	got := taskStatusField(t, stack, applyID, sid, fc2HandlerPlanIdx, fc2Passage)
	if got != "TASK_STATUS_SKIPPED" {
		t.Fatalf("* FC-2 CASCADE DEFECT: run 2 handler (core.service.restarted, "+
			"onchanges:[nginx_default_conf]) status=%q, expected TASK_STATUS_SKIPPED. "+
			"The onchanges source on run 2 is not changed - the handler must NOT execute. "+
			"Triggering it means a spurious nginx restart on every no-op apply. apply=%s sid=%s plan_index=%d",
			got, applyID, sid, fc2HandlerPlanIdx)
	}
}

// assertNotFailedNotSkipped - chain sanity backstop: the task on repeat did not
// fail (FAILED/TIMED_OUT) and was not unexpectedly skipped (SKIPPED - these
// tasks have no when:/onchanges:, SKIPPED would mean broken gating). OK or
// CHANGED are both valid.
func assertNotFailedNotSkipped(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx int, mod string) {
	t.Helper()
	got := taskStatusField(t, stack, applyID, sid, planIdx, fc2Passage)
	switch got {
	case "TASK_STATUS_OK", "TASK_STATUS_CHANGED":
		return
	default:
		t.Fatalf("FC-2 sanity: run 2 %s status=%q, expected OK/CHANGED "+
			"(a task with no gating shouldn't fail/skip on repeat). apply=%s sid=%s plan_index=%d",
			mod, got, applyID, sid, planIdx)
	}
}

// registerChangedField reads register_data->>'changed' for a task (apply_id, sid,
// plan_index) directly from apply_task_register - a local analog of FC-0's
// AssertTaskRegisterField, but returns the value for diagnostic comparison
// (the FC-0 version does t.Fatal on mismatch, we need our own message about the
// "idempotency defect"). Same source - migration 079 apply_task_register.
func registerChangedField(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got string
	err := stack.DB().QueryRow(ctx, `
		SELECT COALESCE(register_data->>'changed', '<null>')
		FROM apply_task_register
		WHERE apply_id = $1 AND sid = $2 AND plan_index = $3
	`, applyID, sid, planIdx).Scan(&got)
	if err != nil {
		t.Fatalf("registerChangedField(apply=%s sid=%s plan_index=%d): no register row "+
			"(real soul didn't return register?): %v", applyID, sid, planIdx, err)
	}
	return strings.TrimSpace(got)
}

// taskStatusField reads payload->>'status' for a task (apply_id, sid, plan_index,
// passage) from audit_log (event_type=task.executed) - a local analog of FC-0's
// AssertTaskStatus, returning the literal for its own comparison. Takes the
// row latest by created_at (same "last wins" as in FC-0).
func taskStatusField(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx, passage int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var status string
	err := stack.DB().QueryRow(ctx, `
		SELECT payload->>'status'
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
		ORDER BY created_at DESC
		LIMIT 1
	`, applyID, sid, planIdx, passage).Scan(&status)
	if err != nil {
		t.Fatalf("taskStatusField(apply=%s sid=%s plan_index=%d passage=%d): no "+
			"task.executed row (task didn't run / TaskEvent didn't arrive?): %v",
			applyID, sid, planIdx, passage, err)
	}
	return strings.TrimSpace(status)
}
