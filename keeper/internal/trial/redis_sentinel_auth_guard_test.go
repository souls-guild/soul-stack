package trial

// Guard on connection-AUTH to sentinel daemon :26379 (prod defect: verify/MONITOR
// tasks of community.redis did not authenticate to sentinel daemon protected by
// aclfile sentinel-users.acl, → apply failed `connect: NOAUTH Authentication required`).
//
// Root: sentinel.conf carries `aclfile sentinel-users.acl`, where default user
// is `user default on #<hash>` (sentinel_users.default = MASTER secret
// secret/<svc>/<inc>#password). So ANY connection to :26379 requires AUTH — both
// SENTINEL MONITOR (community.redis.sentinel) and PONG-verify (community.redis.command
// args PING). connection-AUTH goes via params.password (parseConnConfig →
// redis.Options.Password), not via auth_pass (that is master monitoring password,
// SENTINEL SET <master> auth-pass command, unrelated to daemon AUTH itself).
//
// Why Go guard, not just L0 case.yml: L0 expect_tasks verifies params_subset
// ONLY for MONITOR tasks and ONLY in cases with expect_tasks (PONG-verify not verified
// anywhere, detach_source branch — separate scenario). This guard walks REAL plan
// (LoadScenarioManifest + ExpandIncludes) and catches ENTIRE class: every community.redis
// task with addr on :26379 MUST carry non-empty params.password. Mutation (deletion
// of password from any :26379 task of sentinel scenario) fails this test.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// sentinelDaemonPort — sentinel daemon port; addr with it requires connection-AUTH
// (daemon protected by aclfile, default user `on`). Literal in all sentinel tasks
// (sentinel daemon is not TLS-port agnostic: 26379 without separate tls-port).
const sentinelDaemonPort = "26379"

// scenarioCasesWithSentinelDaemon — L0 cases whose plan contains connection to sentinel
// daemon :26379 (create sentinel branch + detach_source sentinel branch of both services
// with aclfile-protected daemon). loadScenarioPlan loads scenario by path of any of its
// cases; branches are dropped LATER on render, so plan carries both branches and :26379 tasks
// are fully visible on it.
var scenarioCasesWithSentinelDaemon = []string{
	"../../../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml",
	"../../../examples/service/redis/scenario/detach_source/tests/sentinel-detach-source/case.yml",
	"../../../examples/service/dragonfly/scenario/create/tests/create-sentinel-1master-1replica/case.yml",
}

// loadScenarioPlan loads scenario by path of any of its L0 cases and returns flat
// plan after ExpandIncludes (without Stratify — guard checks params of tasks, not order).
// Neutral about scenario name: same mechanism as loadCreatePlan, but not tied to
// create semantics (also used for detach_source).
func loadScenarioPlan(t *testing.T, caseFile string) []config.Task {
	t.Helper()
	_, file, err := LoadCase(caseFile)
	if err != nil {
		t.Fatalf("LoadCase(%s): %v", caseFile, err)
	}
	scnPath := scenarioPathFor(file)
	scn, _, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest(%s): %v", scnPath, err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("scenario invalid: %s", formatDiags(diags))
	}
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if diag.HasErrors(iDiags) {
		t.Fatalf("expand includes: %s", formatDiags(iDiags))
	}
	return expanded
}

// taskAddrParam — literal value of params.addr of task (or "" if addr is not
// string/missing). sentinel tasks set addr as string literal
// "127.0.0.1:26379", so substring of port in it is sufficient.
func taskAddrParam(t *config.Task) string {
	if t.Module == nil {
		return ""
	}
	if s, ok := t.Module.Params["addr"].(string); ok {
		return s
	}
	return ""
}

// taskHasConnectionPassword checks if task carries non-empty params.password (string
// with CEL expression before render — here PRESENCE of field is checked, not resolved
// value: empty string/missing = NOAUTH in prod).
func taskHasConnectionPassword(t *config.Task) bool {
	if t.Module == nil {
		return false
	}
	s, ok := t.Module.Params["password"].(string)
	return ok && strings.TrimSpace(s) != ""
}

// assertSentinelDaemonTasksAuthenticate checks that every community.redis task with addr on
// :26379 in scenario plan carries connection-password.
func assertSentinelDaemonTasksAuthenticate(t *testing.T, caseFile string) {
	t.Helper()
	tasks := loadScenarioPlan(t, caseFile)

	checked := 0
	for i := range tasks {
		task := &tasks[i]
		if task.Module == nil || !strings.HasPrefix(task.Module.Module, "community.redis.") {
			continue
		}
		if !strings.Contains(taskAddrParam(task), sentinelDaemonPort) {
			continue
		}
		checked++
		if !taskHasConnectionPassword(task) {
			t.Fatalf("%s: task %q (%s, addr on :%s) does NOT carry params.password — connection to sentinel daemon is protected by aclfile (default user `on`), without password prod run fails `connect: NOAUTH`",
				caseFile, task.Name, task.Module.Module, sentinelDaemonPort)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no community.redis task on :%s in plan — guard lost its subject of check (sentinel branch stopped connecting to daemon?)",
			caseFile, sentinelDaemonPort)
	}
	t.Logf("%s: %d community.redis tasks on :%s — all carry connection-password", caseFile, checked, sentinelDaemonPort)
}

// TestSentinelDaemonTasksCarryConnectionPassword is a guard for connection-AUTH to :26379
// across all sentinel-bearing scenarios (create + detach_source, redis + dragonfly).
func TestSentinelDaemonTasksCarryConnectionPassword(t *testing.T) {
	for _, caseFile := range scenarioCasesWithSentinelDaemon {
		t.Run(scenarioGuardSubtestName(caseFile), func(t *testing.T) {
			assertSentinelDaemonTasksAuthenticate(t, caseFile)
		})
	}
}

// scenarioGuardSubtestName creates compact subtest name from case path
// (<service>/<scenario>/<case>) for readable output.
func scenarioGuardSubtestName(caseFile string) string {
	parts := strings.Split(caseFile, "/")
	// .../service/<svc>/scenario/<scn>/tests/<case>/case.yml
	for i := range parts {
		if parts[i] == "service" && i+4 < len(parts) {
			return fmt.Sprintf("%s_%s_%s", parts[i+1], parts[i+3], parts[i+5])
		}
	}
	return caseFile
}
