//go:build e2e_live

// L3b live gate for SoulModule plugin delivery (NIM-32 S2, ADR-065 S5, a live
// guarantee for NIM-8 auto-synthesis). The initiative's main test: proves ON A
// LIVE stand (keeper + PG/Redis/Vault + a genuine soul container) the full
// core.module.installed path, which L0/integration only cover keeper-side
// (module_installs_integration_test.go - a dispatched plan without a real Soul).
//
// Fixture tests/e2e-live/module-delivery-live: service.yml declares
// community.redis in modules[] and does NOT contain an explicit install step.
// Synthesis, fetch, verify, atomic-rename into the host slot, and hot-register
// are all observed on the wire.
//
// Order (also the assert chain):
//
//	create(assert 1) -> verify_live before allow(assert 2) -> allow+unlock ->
//	verify_live(assert 3,4) -> host slot(assert 5) -> verify_live repeat(assert 6).
//
// Per-task statuses are read from audit_log (event_type=task.executed) by
// GLOBAL plan_index - the only per-task persistence surface of keeper (see the
// harness/asserts.go doc block FC-0). Synthesis-step identity is proved by
// error.module=core.module in the negative case (assert 2): plan_index 0 = the
// synthesized core.module.installed, plan_index 1 = the consumer (structure
// confirmed by module_installs_integration_test.go::TestIntegration_ModuleInstallSynthesis).
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	moduleDeliverySID  = "soul-live-a.example.com"
	moduleDeliverySlot = "/var/lib/soul-stack/modules/community-redis" // ADR-065(g) <paths.modules>/<ns>-<name>
	// createAuthoredTasks - number of authored tasks in scenario/create (pkg+service),
	// baseline for assert 1 (create plan without synthesis). Bump when adding a
	// task to scenario/create/main.yml.
	createAuthoredTasks = 2
)

func TestL3bModuleDeliveryLive_SynthesisFetchHotRegister(t *testing.T) {
	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/module-delivery-live",
		ServiceName: "module-delivery-live",
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: "redis", Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	sid := stack.SoulContainers[0].SID
	if sid != moduleDeliverySID {
		t.Fatalf("SoulContainers[0].SID = %q, expected %q", sid, moduleDeliverySID)
	}

	const incName = "module-delivery"

	// Coven membership BEFORE create: the roster resolves via incarnation.name ∈ coven[]
	// (ADR-008). WaitSoulprintReported - soul fully online before the day-2 roster resolve.
	stack.AddSoulToCoven(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	// -- create: redis via core modules, WITHOUT the community.redis consumer --
	// 300s - apt-get update + install redis-server on a fresh Debian-12 (like redis-live).
	inc, createApply := stack.CreateIncarnationWithApply(t, incName, "module-delivery-live@main", nil)
	stack.WaitApplySuccess(t, createApply, 300)
	stack.WaitIncarnationReady(t, inc, 30)

	// -- ASSERT 1: create plan WITHOUT a synthesis step (ADR-065(e)) ----------
	// modules[] is declared, but create doesn't use community.redis -> no synthesis.
	// A direct check "no core.module.installed in the plan" is IMPOSSIBLE: the
	// module name/address in audit_log task.executed only appears inside error{}
	// (filled on FAILED, shared/audit.BuildTaskExecutedPayload), and create is
	// all SUCCESS -> its tasks have no module fields. So we count tasks: create
	// = EXACTLY createAuthoredTasks (pkg+service), synthesis would add a third
	// distinct plan_index. Transitive safety net: synthesis in create -> install
	// without an active allow would fail-close with module_not_allowed ->
	// error_locked -> WaitApplySuccess above would already have failed.
	if n := distinctTaskCount(t, stack, createApply, sid); n != createAuthoredTasks {
		t.Fatalf("assert1: create run = %d distinct plan_index, expected %d (extra = injected core.module.installed without a consumer)", n, createAuthoredTasks)
	}

	// -- ASSERT 2: fail-closed BEFORE allow (ADR-065(f)) ----------------------
	// verify_live calls community.redis.command -> keeper synthesizes
	// core.module.installed (plan_index 0) BEFORE the consumer (plan_index 1)
	// and dispatches the plan WITHOUT an active Sigil (keeper doesn't fail-fast,
	// ADR-065(e)). The soul-side allow-check fails the install step with
	// module_not_allowed before a single byte moves.
	failApply := stack.RunScenario(t, incName, "verify_live", nil)
	stack.WaitIncarnationStatus(t, incName, "error_locked", 120)

	code, mod, msg := taskErrorByPlan(t, stack, failApply, sid, 0)
	// module Failed event -> TaskError.Code=module.failed, Module=core.module
	// (SplitModuleAddr strips state), reason module_not_allowed - prefix of
	// message (applyrunner.go, installed.go). error.module=core.module proves:
	// plan_index 0 = the synthesized core.module.installed.
	if code != "module.failed" {
		t.Fatalf("assert2: install step (plan_index 0) error.code=%q, expected module.failed (fail-closed)", code)
	}
	if mod != "core.module" {
		t.Fatalf("assert2: install step error.module=%q, expected core.module (this is the synthesis step)", mod)
	}
	if !strings.Contains(msg, "module_not_allowed") || !strings.Contains(msg, "community.redis") {
		t.Fatalf("assert2: install step error.message=%q, expected module_not_allowed + community.redis", msg)
	}
	// NO fetch bytes: allow-check happens BEFORE fetch -> the host slot is NOT materialized.
	assertHostFileAbsent(t, stack, 0, moduleDeliverySlot)

	// -- allow + unlock + repeat verify_live -----------------------------------
	// AllowSoulModule (keeper-side seal) -> active Sigil. Unlock clears
	// error_locked after the intentional failure (otherwise lockRun rejects the repeat).
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)
	stack.Unlock(t, incName, "e2e NIM-32: unlock after negative fail-closed")

	okApply := stack.RunScenario(t, incName, "verify_live", nil)
	// 180s - FetchModule (bytes Keeper->Soul) + verify + hot-register + go-plugin
	// PING. Faster than create (no apt), but with margin for the go-plugin cold start.
	stack.WaitApplySuccess(t, okApply, 180)
	stack.WaitIncarnationReady(t, inc, 30)

	// -- ASSERT 3: install step BEFORE the consumer, changed=true (first install) --
	// plan_index 0 = synthesis-install (identity - assert 2: error.module=core.module
	// on the same plan_index); comes BEFORE the consumer (plan_index 1). First
	// install (disk sha256 != allow) -> CHANGED.
	if st := taskStatusByPlan(t, stack, okApply, sid, 0); st != "TASK_STATUS_CHANGED" {
		t.Fatalf("assert3: install step (plan_index 0) status=%q, expected TASK_STATUS_CHANGED (first install)", st)
	}

	// -- ASSERT 4: consumer SUCCESS in the SAME run (hot-register), PONG -------
	// community.redis.command (plan_index 1) ran AFTER install in the same run
	// without a daemon restart (ADR-065(d)). changed=false -> OK; register
	// result=PONG proves the live Redis responded (the failed_when gate passed).
	if st := taskStatusByPlan(t, stack, okApply, sid, 1); st != "TASK_STATUS_OK" {
		t.Fatalf("assert4: consumer community.redis.command (plan_index 1) status=%q, expected TASK_STATUS_OK", st)
	}
	stack.AssertTaskRegisterField(t, okApply, sid, 1, "result", "PONG")

	// -- ASSERT 5: host slot layout ADR-065(g) ---------------------------------
	// <paths.modules>/community-redis/{manifest.yaml, soul-mod-redis}; the binary is executable.
	stack.AssertHostFileExists(t, 0, moduleDeliverySlot+"/manifest.yaml")
	stack.AssertHostFileExists(t, 0, moduleDeliverySlot+"/soul-mod-redis")
	assertHostFileExecutable(t, stack, 0, moduleDeliverySlot+"/soul-mod-redis")

	// -- ASSERT 6: idempotency of a repeat run (ADR-065(c)) --------------------
	// sha256 of the installed binary == active allow -> install changed=false,
	// fetch does NOT run; consumer SUCCESS again. incarnation ready after
	// okApply -> a regular RunScenario (no unlock).
	idemApply := stack.RunScenario(t, incName, "verify_live", nil)
	stack.WaitApplySuccess(t, idemApply, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	if st := taskStatusByPlan(t, stack, idemApply, sid, 0); st != "TASK_STATUS_OK" {
		t.Fatalf("assert6: install step (plan_index 0) on repeat status=%q, expected TASK_STATUS_OK (idempotent, changed=false)", st)
	}
	if st := taskStatusByPlan(t, stack, idemApply, sid, 1); st != "TASK_STATUS_OK" {
		t.Fatalf("assert6: consumer (plan_index 1) on repeat status=%q, expected TASK_STATUS_OK", st)
	}
	stack.AssertTaskRegisterField(t, idemApply, sid, 1, "result", "PONG")
}

// distinctTaskCount - number of DISTINCT plan_index values in task.executed for
// a run (= number of executed tasks). More than the scenario's authored task
// count means an injected synthesis step. Reads audit_log - keeper's only
// per-task persistence.
func distinctTaskCount(t *testing.T, s *harness.Stack, applyID, sid string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := s.DB().QueryRow(ctx, `
		SELECT COUNT(DISTINCT payload->>'plan_index')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1 AND payload->>'sid' = $2
	`, applyID, sid).Scan(&n)
	if err != nil {
		t.Fatalf("distinctTaskCount(apply=%s sid=%s): %v", applyID, sid, err)
	}
	return n
}

// taskStatusByPlan - the TaskStatus literal for a task by (applyID, sid,
// plan_index), latest by created_at (retry/cross-keeper duplicate). plan_index
// is globally unique across Passage (migration 079) -> no per-passage filter needed.
func taskStatusByPlan(t *testing.T, s *harness.Stack, applyID, sid string, planIdx int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var status string
	err := s.DB().QueryRow(ctx, `
		SELECT payload->>'status'
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1 AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		ORDER BY created_at DESC LIMIT 1
	`, applyID, sid, planIdx).Scan(&status)
	if err != nil {
		t.Fatalf("taskStatusByPlan(apply=%s sid=%s plan_index=%d): no task.executed row: %v", applyID, sid, planIdx, err)
	}
	return status
}

// taskErrorByPlan returns task error.code/module/message by plan_index
// (fail-closed negative). Empty fields become "<no-error>"/"<no-module>"/"" markers.
func taskErrorByPlan(t *testing.T, s *harness.Stack, applyID, sid string, planIdx int) (code, module, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.DB().QueryRow(ctx, `
		SELECT COALESCE(payload->'error'->>'code','<no-error>'),
		       COALESCE(payload->'error'->>'module','<no-module>'),
		       COALESCE(payload->'error'->>'message','')
		FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1 AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		ORDER BY created_at DESC LIMIT 1
	`, applyID, sid, planIdx).Scan(&code, &module, &message)
	if err != nil {
		t.Fatalf("taskErrorByPlan(apply=%s sid=%s plan_index=%d): no task.executed row: %v", applyID, sid, planIdx, err)
	}
	return code, module, message
}

// assertHostFileExecutable checks file exists and has x bit (host binary slot for
// soul-mod-redis, ADR-065(g)). Reusing assertHostFileAbsent is not enough:
// `test -x` is required.
func assertHostFileExecutable(t *testing.T, s *harness.Stack, soulIdx int, path string) {
	t.Helper()
	sc := s.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := sc.Exec(ctx, []string{"test", "-x", path})
	if err != nil {
		t.Fatalf("assertHostFileExecutable(%s): exec: %v\noutput=%s", path, err, out)
	}
	if code != 0 {
		t.Fatalf("assertHostFileExecutable(%s): not executable (test -x exit=%d)", path, code)
	}
}
