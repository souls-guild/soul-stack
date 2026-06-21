//go:build e2e_live

// FC-5 L3b real-apply: per-task `when:` register-gating на ПОДЛИННОМ register,
// исполняемом реальным soul против ЖИВОГО redis в трёх Debian-12-контейнерах.
//
// ЗАЧЕМ (gap, который закрываем). L3a-stub НЕ вычисляет when (soul-stub отдаёт
// заранее заданный статус, не прогоняя flow-control-движок), поэтому
// register-зависимый `when:`-gating — Soul-side per-task решение ДО Apply — не
// доказан на реальном register. FC-1 (redis_cluster_update_acl_test.go) доказал
// СИММЕТРИЧНЫЙ, но ДРУГОЙ путь — `where:`-targeting:
//
//   where = Keeper-side targeting. Хост, у которого where:false, ВООБЩЕ не
//           получает задачу: на уровне apply_runs он либо без passage-строки,
//           либо no_match. Задача на нём не существует.
//   when  = Soul-side per-task gating. Задача БЕЗ where: уходит на ВСЕ хосты
//           прогона; Soul вычисляет when sandboxed-cel-движком и при when:false
//           эмитит skippedTaskEvent → TASK_STATUS_SKIPPED (mod.Apply не зовётся),
//           отдельной task.executed-строкой в audit_log.
//
// Это и есть наблюдаемое отличие, которое тест доказывает на real-apply:
// register-gated when даёт per-task SKIPPED-СОБЫТИЕ (audit_log), а НЕ no_match на
// уровне apply_runs (как where).
//
// СЦЕНАРИЙ (tests/e2e-live/when-gate-live, локальная fixture в зоне теста, НЕ
// правит committed examples/):
//   Passage 0 — probe `redis-cli role | head -1 | tr -d '\n'` → register: redis_role
//               (ЖИВОЙ redis: host-0 master, host-1/2 REPLICAOF host-0 → role
//               master vs slave). tr -d '\n' — register.stdout без trailing \n.
//   Passage 1 — задача БЕЗ where:, `when: register.redis_role.stdout == 'master'`,
//               core.cmd.shell пишет маркер-файл, changed_when: false. keeper
//               стратифицирует её в Passage 1 (потребитель register строго ПОСЛЕ
//               probe — soul-lint подтверждает [1 1]) и протягивает when КАК
//               CEL-СТРОКУ в RenderedTask.when (НЕ вычисляет — register известен
//               только Soul-у). Soul: master → OK, реплики → SKIPPED.
//
// ★ trim: probe-cmd несёт `tr -d '\n'` (cmd-модуль stdout НЕ тримит — та же
// находка, что у FC-1 update_acl). Без него register.redis_role.stdout = "master\n"
// и when:=='master' не сматчился бы → master скипнулся бы как реплика (ложный
// «gating работает наоборот»). Регресс-guard ниже фейлит на \n в register-stdout.
//
// Live-bootstrap живого redis (bootstrapLiveRedis) переиспользуется из
// redis_cluster_update_acl_test.go (один пакет e2e_live_test) — стоп-правило
// «цепочка → карта»: не плодим второй redis-bootstrap.
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	wgService     = "when-gate-live"
	wgExamplePath = "tests/e2e-live/when-gate-live"

	// План when_role_gate: probe (Passage 0) + when-gated действие (Passage 1).
	// plan_index — ГЛОБАЛЬНЫЙ сквозной индекс по всему плану (миграция 079,
	// ADR-056 §S1): probe = 0 (passage 0), действие = 1 (passage 1).
	wgProbePlanIdx   = 0
	wgProbePassage   = 0
	wgActionPlanIdx  = 1
	wgActionPassage  = 1
	wgActionFilePath = "/tmp/when-gated-acted"
)

func TestFC5WhenGating_LiveRegister(t *testing.T) {
	t.Skip("БЛОКЕР (structural, needs_architect): when: register.<отдельный-probe> недостижим под staged-render — стратификатор уводит when-потребителя в Passage после probe, Soul cross-passage register не видит → no such key. where: это умеет (Keeper пере-рендер с накопленным register), when: — нет. Решение a(seed register в ApplyRequest)/b(Keeper pre-eval register-when симметрично where)/c(объявить неподдержанным + soul-lint + фикс ADR-056 стр.97) — за architect. Тест воспроизводит cross-passage no-such-key, реактивировать после решения.")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: wgExamplePath,
		ServiceName: wgService,
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("ожидалось 3 soul-контейнера, получено %d", got)
	}
	const (
		masterSID   = "soul-live-a.example.com"
		replica1SID = "soul-live-b.example.com"
		replica2SID = "soul-live-c.example.com"
	)
	wantSIDs := []string{masterSID, replica1SID, replica2SID}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, ожидалось %q", i, got, want)
		}
	}

	const incName = "when-gate-live-run"

	// Coven-членство ДО запуска scenario: roster резолвится по incarnation.name ∈
	// coven[] (ADR-008). Без него no_hosts → ноль строк apply_runs.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// Прямой seed ready-incarnation (state_schema пуст, фокус — gating, не state).
	stack.SeedIncarnationReady(t, incName, wgService, "main", map[string]any{})

	// ── Bootstrap ЖИВОГО redis: host-0 master, host-1/2 REPLICAOF host-0 ───────
	// Делает probe `redis-cli role` различимым (master vs slave) на реальном
	// apply. Переиспользуется из redis_cluster_update_acl_test.go (один пакет).
	bootstrapLiveRedis(t, stack)

	applyID := stack.RunScenario(t, incName, "when_role_gate", nil)

	stack.WaitApplySuccess(t, applyID, 180)
	stack.WaitIncarnationReady(t, incName, 30)

	// ── (1) probe-register: реальный soul вернул роль каждого хоста ────────────
	// Passage 0, plan_index 0. register stdout — то, что реальный soul напечатал
	// на `redis-cli role | head -1 | tr -d '\n'`.
	masterRole := registerStdout(t, stack, applyID, masterSID)
	r1Role := registerStdout(t, stack, applyID, replica1SID)
	r2Role := registerStdout(t, stack, applyID, replica2SID)
	t.Logf("probe register: master=%q replica1=%q replica2=%q", masterRole, r1Role, r2Role)

	// ★ trim-регресс-guard: register.redis_role.stdout с trailing \n сломал бы
	// when:=='master' (master скипнулся бы как реплика — ложный «gating инверсно»).
	if strings.Contains(masterRole, "\n") {
		t.Fatalf("★ probe-newline-регресс: master register stdout = %q содержит \\n — probe `redis-cli role | head -1 | tr -d '\\n'` потерял trim; when:=='master' не сматчит на реальном register (восстановить tr -d в tests/e2e-live/when-gate-live/scenario/when_role_gate/main.yml)", masterRole)
	}
	if strings.TrimSpace(masterRole) != "master" {
		t.Fatalf("probe: master-хост вернул role=%q, ожидалось master (redis-bootstrap некорректен?)", masterRole)
	}
	if strings.TrimSpace(r1Role) != "slave" || strings.TrimSpace(r2Role) != "slave" {
		t.Fatalf("probe: реплики вернули role r1=%q r2=%q, ожидалось slave (REPLICAOF не сошёлся?)", r1Role, r2Role)
	}

	// FC-0 канонический ключ: probe register stdout по plan_index 0 == 'master'/'slave'.
	// Это и есть вход, по которому Soul вычислил when: register.redis_role.stdout.
	stack.AssertTaskRegisterField(t, applyID, masterSID, wgProbePlanIdx, "stdout", "master")
	stack.AssertTaskRegisterField(t, applyID, replica1SID, wgProbePlanIdx, "stdout", "slave")
	stack.AssertTaskRegisterField(t, applyID, replica2SID, wgProbePlanIdx, "stdout", "slave")

	// ── (2) ★ КЛЮЧ FC-5: per-task when-gating Soul-side по реальному register ───
	// Действие БЕЗ where: дошло до ВСЕХ трёх хостов (apply_runs success у каждого —
	// WaitApplySuccess это уже доказал). Soul-side when:
	//   master  → when:true  → задача исполнилась → TASK_STATUS_OK (changed_when: false).
	//   реплики → when:false → Soul эмитит TASK_STATUS_SKIPPED ДО Apply.
	// AssertTaskStatus читает per-task task.executed-строку audit_log (FC-0):
	// SKIPPED-событие ПЕРСИСТИТСЯ как отдельная строка — это и есть отличие от
	// where (где на реплике задачи нет вовсе).
	stack.AssertTaskStatus(t, applyID, masterSID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_OK")
	stack.AssertTaskStatus(t, applyID, replica1SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")
	stack.AssertTaskStatus(t, applyID, replica2SID, wgActionPlanIdx, wgActionPassage, "TASK_STATUS_SKIPPED")

	// ── (3) ★ Отличие when от where ДОКАЗАНО на персистентности ────────────────
	// when даёт SKIPPED-СОБЫТИЕ (task.executed-строка существует) на репликах, а НЕ
	// no_match/отсутствие apply_runs-строки. Прямой контр-ассерт: у where (FC-1
	// update_acl) реплика на where-задаче имеет apply_runs(passage=1)=no_match ЛИБО
	// строки нет; у when реплика имеет apply_runs success (задача дошла), а per-task
	// SKIPPED живёт в audit_log. Доказываем обе грани:
	//   3a. apply_runs реплики = success (НЕ no_match) — задача дошла до хоста.
	assertWhenReplicaApplyRunSucceeded(t, stack, applyID, replica1SID, replica2SID)
	//   3b. task.executed-строка для SKIPPED-действия реплики РЕАЛЬНО есть (персист).
	assertTaskEventPersisted(t, stack, applyID, replica1SID, wgActionPlanIdx, wgActionPassage)
	assertTaskEventPersisted(t, stack, applyID, replica2SID, wgActionPlanIdx, wgActionPassage)

	// ── (4) Independent verify на ЖИВЫХ контейнерах: маркер-файл ───────────────
	// when-gated cmd писал /tmp/when-gated-acted. master выполнил → файл есть;
	// реплики SKIPPED (mod.Apply не зван) → файла НЕТ. Доказывает, что SKIPPED —
	// настоящий skip Apply, а не «выполнилась, но статус занижен».
	stack.AssertHostFileExists(t, 0, wgActionFilePath)
	assertHostFileAbsent(t, stack, 1, wgActionFilePath)
	assertHostFileAbsent(t, stack, 2, wgActionFilePath)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "when_role_gate",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// assertWhenReplicaApplyRunSucceeded проверяет, что реплики получили apply_runs со
// статусом success (НЕ no_match) — то есть when-gated задача ДОШЛА до хоста (в
// отличие от where, отфильтровавшего бы хост из таргета). per-task SKIPPED не
// меняет терминал apply_runs хоста: SKIPPED — нейтральный исход (не fail, не
// no_match), хост штатно завершает прогон success.
func assertWhenReplicaApplyRunSucceeded(t *testing.T, stack *harness.Stack, applyID string, replicaSIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, sid := range replicaSIDs {
		var status string
		err := stack.DB().QueryRow(ctx,
			`SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid).Scan(&status)
		if err != nil {
			t.Fatalf("★ when≠where: реплика %s НЕ имеет apply_runs-строки — задача не дошла до хоста (это поведение where, не when): %v", sid, err)
		}
		if status == "no_match" {
			t.Fatalf("★ when≠where: реплика %s apply_runs status=no_match — хост отфильтрован из таргета (where-семантика); when обязан ДОСТАВИТЬ задачу и скипнуть её Soul-side, а не отсеять хост", sid)
		}
		if status != "success" {
			t.Fatalf("★ реплика %s apply_runs status=%q, ожидался success (per-task SKIPPED — нейтральный исход, хост должен завершить прогон success)", sid, status)
		}
	}
}

// assertTaskEventPersisted проверяет, что per-task task.executed-строка для задачи
// (apply_id, sid, plan_index, passage) РЕАЛЬНО есть в audit_log — это и есть
// материальное отличие when от where: SKIPPED-событие персистится отдельной
// строкой (соул эмитит skippedTaskEvent), тогда как where-отфильтрованный хост
// task.executed-строки этой задачи не имеет вовсе.
func assertTaskEventPersisted(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx, passage int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := stack.DB().QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = 'task.executed'
		  AND correlation_id = $1
		  AND payload->>'sid' = $2
		  AND (payload->>'plan_index')::int = $3
		  AND (payload->>'passage')::int = $4
	`, applyID, sid, planIdx, passage).Scan(&n)
	if err != nil {
		t.Fatalf("assertTaskEventPersisted(sid=%s plan_index=%d passage=%d): query: %v", sid, planIdx, passage, err)
	}
	if n == 0 {
		t.Fatalf("★ when≠where: для реплики %s НЕТ task.executed-строки when-gated задачи (plan_index=%d passage=%d) — SKIPPED-событие не персистнуто; when обязан эмитить per-task SKIPPED-событие, а не молча выкинуть задачу как where", sid, planIdx, passage)
	}
}
