package trial

// Guard на connection-AUTH к sentinel-демону :26379 (прод-дефект: verify/MONITOR-
// задачи community.redis НЕ аутентифицировались к sentinel-демону, защищённому
// aclfile sentinel-users.acl, → apply падал `connect: NOAUTH Authentication required`).
//
// Корень: sentinel.conf несёт `aclfile sentinel-users.acl`, где default-юзер
// `user default on #<hash>` (sentinel_users.default = ГЛАВНЫЙ секрет
// secret/<svc>/<inc>#password). Значит ЛЮБОЙ коннект к :26379 требует AUTH — и
// SENTINEL MONITOR (community.redis.sentinel), и PONG-verify (community.redis.command
// args PING). connection-AUTH идёт через params.password (parseConnConfig →
// redis.Options.Password), а НЕ через auth_pass (тот — пароль МОНИТОРИНГА master-а,
// команда SENTINEL SET <master> auth-pass, к AUTH самого демона отношения не имеет).
//
// Почему Go-guard, а не только L0 case.yml: L0 expect_tasks сверяет params_subset
// ТОЛЬКО для MONITOR-задач и ТОЛЬКО в кейсах с expect_tasks (PONG-verify не сверяется
// нигде, detach_source-ветка — отдельный сценарий). Этот guard обходит РЕАЛЬНЫЙ план
// (LoadScenarioManifest + ExpandIncludes) и ловит ВЕСЬ класс: каждая community.redis-
// задача с addr на :26379 ОБЯЗАНА нести непустой params.password. Мутация (удаление
// password из любой :26379-задачи sentinel-сценария) роняет этот тест.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// sentinelDaemonPort — порт sentinel-демона; addr с ним требует connection-AUTH
// (демон защищён aclfile, default-юзер `on`). Литерал во всех sentinel-задачах
// (sentinel-демон не TLS-порт-агностичен: 26379 без отдельного tls-порта).
const sentinelDaemonPort = "26379"

// scenarioCasesWithSentinelDaemon — L0-кейсы, чей план содержит коннект к sentinel-
// демону :26379 (create sentinel-ветка + detach_source sentinel-ветка обоих сервисов
// с aclfile-защищённым демоном). loadScenarioPlan грузит сценарий по пути любого его
// кейса; ветви дропаются ПОЗЖЕ на render, поэтому план несёт обе ветви и :26379-задачи
// видны на нём целиком.
var scenarioCasesWithSentinelDaemon = []string{
	"../../../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml",
	"../../../examples/service/redis/scenario/detach_source/tests/sentinel-detach-source/case.yml",
	"../../../examples/service/dragonfly/scenario/create/tests/create-sentinel-1master-1replica/case.yml",
}

// loadScenarioPlan грузит сценарий по пути любого его L0-кейса и возвращает плоский
// план после ExpandIncludes (без Stratify — guard сверяет params задач, не порядок).
// Нейтрально по имени сценария: тот же механизм, что loadCreatePlan, но не привязан к
// create-семантике (используется и для detach_source).
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

// taskAddrParam — литеральное значение params.addr задачи (или "" если addr не
// строка/отсутствует). sentinel-задачи задают addr строковым литералом
// "127.0.0.1:26379", поэтому подстроки порта в нём достаточно.
func taskAddrParam(t *config.Task) string {
	if t.Module == nil {
		return ""
	}
	if s, ok := t.Module.Params["addr"].(string); ok {
		return s
	}
	return ""
}

// taskHasConnectionPassword — задача несёт непустой params.password (строка с CEL-
// выражением до render — здесь проверяется ПРИСУТСТВИЕ поля, не резолвнутое значение:
// пустая строка/отсутствие = NOAUTH в проде).
func taskHasConnectionPassword(t *config.Task) bool {
	if t.Module == nil {
		return false
	}
	s, ok := t.Module.Params["password"].(string)
	return ok && strings.TrimSpace(s) != ""
}

// assertSentinelDaemonTasksAuthenticate — каждая community.redis-задача с addr на
// :26379 в плане сценария несёт connection-password.
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
			t.Fatalf("%s: задача %q (%s, addr на :%s) НЕ несёт params.password — коннект к sentinel-демону защищён aclfile (default-юзер `on`), без password прод-прогон падает `connect: NOAUTH`",
				caseFile, task.Name, task.Module.Module, sentinelDaemonPort)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: ни одной community.redis-задачи на :%s в плане — guard потерял предмет проверки (sentinel-ветка перестала коннектиться к демону?)",
			caseFile, sentinelDaemonPort)
	}
	t.Logf("%s: %d community.redis-задач на :%s — все несут connection-password", caseFile, checked, sentinelDaemonPort)
}

// TestSentinelDaemonTasksCarryConnectionPassword — guard на connection-AUTH к :26379
// по всем sentinel-несущим сценариям (create + detach_source, redis + dragonfly).
func TestSentinelDaemonTasksCarryConnectionPassword(t *testing.T) {
	for _, caseFile := range scenarioCasesWithSentinelDaemon {
		t.Run(scenarioGuardSubtestName(caseFile), func(t *testing.T) {
			assertSentinelDaemonTasksAuthenticate(t, caseFile)
		})
	}
}

// scenarioGuardSubtestName — компактное имя под-теста из пути кейса
// (<service>/<scenario>/<case>) для читаемого вывода.
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
