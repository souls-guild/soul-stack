//go:build e2e_live

// L3b live-гейт доставки SoulModule-плагинов (NIM-32 S2, ADR-065 S5, живая
// гарантия NIM-8 auto-синтеза). Главный тест инициативы: доказывает НА ЖИВОМ
// стенде (keeper + PG/Redis/Vault + подлинный soul-контейнер) весь путь
// core.module.installed, который L0/integration покрывают только keeper-side
// (module_installs_integration_test.go — dispatched-план без реального Soul).
//
// Fixture tests/e2e-live/module-delivery-live: service.yml декларирует
// community.redis в modules[] и НЕ содержит явного install-шага. Синтез, fetch,
// verify, atomic-rename в host-слот и hot-register — всё наблюдается на wire.
//
// Порядок (он же — цепочка ассертов):
//
//	create(assert 1) → verify_live до allow(assert 2) → allow+unlock →
//	verify_live(assert 3,4) → host-слот(assert 5) → verify_live повтор(assert 6).
//
// Per-task статусы читаются из audit_log (event_type=task.executed) по
// ГЛОБАЛЬНОМУ plan_index — единственная per-task persistence-поверхность keeper-а
// (см. harness/asserts.go doc-блок FC-0). Идентичность синтез-шага доказывается
// error.module=core.module в негативе (assert 2): plan_index 0 = синтезированный
// core.module.installed, plan_index 1 = потребитель (структуру подтверждает
// module_installs_integration_test.go::TestIntegration_ModuleInstallSynthesis).
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
	// createAuthoredTasks — число авторских задач scenario/create (pkg+service),
	// база assert 1 (create-план без синтеза). Поднять при добавлении задачи в
	// scenario/create/main.yml.
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
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	sid := stack.SoulContainers[0].SID
	if sid != moduleDeliverySID {
		t.Fatalf("SoulContainers[0].SID = %q, ожидалось %q", sid, moduleDeliverySID)
	}

	const incName = "module-delivery"

	// Coven-членство ДО create: roster резолвится по incarnation.name ∈ coven[]
	// (ADR-008). WaitSoulprintReported — soul полностью онлайн до roster-резолва.
	stack.AddSoulToCoven(t, 0, incName)
	stack.WaitSoulprintReported(t, 0, 60)

	// ── create: redis core-модулями, БЕЗ community.redis-потребителя ──────────
	// 300 c — apt-get update + install redis-server в свежем Debian-12 (как redis-live).
	inc, createApply := stack.CreateIncarnationWithApply(t, incName, "module-delivery-live@main", nil)
	stack.WaitApplySuccess(t, createApply, 300)
	stack.WaitIncarnationReady(t, inc, 30)

	// ── АССЕРТ 1: create-план БЕЗ синтез-шага (ADR-065(e)) ────────────────────
	// modules[] объявлен, но create не использует community.redis → синтеза нет.
	// Прямая проверка «в плане нет core.module.installed» НЕВОЗМОЖНА: имя/адрес
	// модуля в audit_log task.executed есть ТОЛЬКО внутри error{} (заполняется на
	// FAILED, shared/audit.BuildTaskExecutedPayload), а create весь SUCCESS → у его
	// задач module-поля нет. Поэтому считаем задачи: create = РОВНО
	// createAuthoredTasks (pkg+service), синтез добавил бы третий distinct
	// plan_index. Транзитивная страховка: синтез в create → install без активного
	// допуска фейл-клоузнулся бы module_not_allowed → error_locked →
	// WaitApplySuccess выше уже упал бы.
	if n := distinctTaskCount(t, stack, createApply, sid); n != createAuthoredTasks {
		t.Fatalf("assert1: create-прогон = %d distinct plan_index, ожидалось %d (лишний = впрыснутый core.module.installed без потребителя)", n, createAuthoredTasks)
	}

	// ── АССЕРТ 2: fail-closed ДО allow (ADR-065(f)) ───────────────────────────
	// verify_live зовёт community.redis.command → keeper синтезирует
	// core.module.installed (plan_index 0) ПЕРЕД потребителем (plan_index 1) и
	// диспатчит план БЕЗ активного Sigil (keeper не fail-fast-ит, ADR-065(e)).
	// Soul-side allow-check фейлит install-шаг module_not_allowed ДО единого байта.
	failApply := stack.RunScenario(t, incName, "verify_live", nil)
	stack.WaitIncarnationStatus(t, incName, "error_locked", 120)

	code, mod, msg := taskErrorByPlan(t, stack, failApply, sid, 0)
	// module Failed-событие → TaskError.Code=module.failed, Module=core.module
	// (SplitModuleAddr отрезает state), reason module_not_allowed — префикс message
	// (applyrunner.go, installed.go). error.module=core.module доказывает: plan_index
	// 0 = синтезированный core.module.installed.
	if code != "module.failed" {
		t.Fatalf("assert2: install-шаг (plan_index 0) error.code=%q, ожидался module.failed (fail-closed)", code)
	}
	if mod != "core.module" {
		t.Fatalf("assert2: install-шаг error.module=%q, ожидался core.module (это и есть синтез-шаг)", mod)
	}
	if !strings.Contains(msg, "module_not_allowed") || !strings.Contains(msg, "community.redis") {
		t.Fatalf("assert2: install-шаг error.message=%q, ожидалось module_not_allowed + community.redis", msg)
	}
	// БЕЗ fetch-байт: allow-check ДО fetch → host-слот НЕ материализован.
	assertHostFileAbsent(t, stack, 0, moduleDeliverySlot)

	// ── allow + unlock + повтор verify_live ──────────────────────────────────
	// AllowSoulModule (keeper-side seal) → активный Sigil. Unlock снимает
	// error_locked после намеренного fail (иначе lockRun отклонит повтор).
	stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)
	stack.Unlock(t, incName, "e2e NIM-32: разблокировать после негативного fail-closed")

	okApply := stack.RunScenario(t, incName, "verify_live", nil)
	// 180 c — FetchModule (bytes Keeper→Soul) + verify + hot-register + go-plugin
	// PING. Быстрее create (без apt), но с запасом на go-plugin cold-start.
	stack.WaitApplySuccess(t, okApply, 180)
	stack.WaitIncarnationReady(t, inc, 30)

	// ── АССЕРТ 3: install-шаг ПЕРЕД потребителем, changed=true (первая установка) ─
	// plan_index 0 = синтез-install (идентичность — assert 2: error.module=core.module
	// на том же plan_index); стоит ПЕРЕД потребителем (plan_index 1). Первая
	// установка (sha256 диска ≠ допуск) → CHANGED.
	if st := taskStatusByPlan(t, stack, okApply, sid, 0); st != "TASK_STATUS_CHANGED" {
		t.Fatalf("assert3: install-шаг (plan_index 0) status=%q, ожидался TASK_STATUS_CHANGED (первая установка)", st)
	}

	// ── АССЕРТ 4: потребитель SUCCESS в ТОМ ЖЕ прогоне (hot-register), PONG ────
	// community.redis.command (plan_index 1) исполнился ПОСЛЕ install в этом же
	// прогоне без рестарта демона (ADR-065(d)). changed=false → OK; register
	// result=PONG доказывает, что живой Redis ответил (failed_when-гейт пропустил).
	if st := taskStatusByPlan(t, stack, okApply, sid, 1); st != "TASK_STATUS_OK" {
		t.Fatalf("assert4: потребитель community.redis.command (plan_index 1) status=%q, ожидался TASK_STATUS_OK", st)
	}
	stack.AssertTaskRegisterField(t, okApply, sid, 1, "result", "PONG")

	// ── АССЕРТ 5: host-слот раскладки ADR-065(g) ──────────────────────────────
	// <paths.modules>/community-redis/{manifest.yaml, soul-mod-redis}; бинарь исполняемый.
	stack.AssertHostFileExists(t, 0, moduleDeliverySlot+"/manifest.yaml")
	stack.AssertHostFileExists(t, 0, moduleDeliverySlot+"/soul-mod-redis")
	assertHostFileExecutable(t, stack, 0, moduleDeliverySlot+"/soul-mod-redis")

	// ── АССЕРТ 6: идемпотентность повторного прогона (ADR-065(c)) ─────────────
	// sha256 установленного == активный допуск → install changed=false, fetch НЕ
	// выполняется; потребитель снова SUCCESS. incarnation ready после okApply →
	// обычный RunScenario (без unlock).
	idemApply := stack.RunScenario(t, incName, "verify_live", nil)
	stack.WaitApplySuccess(t, idemApply, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	if st := taskStatusByPlan(t, stack, idemApply, sid, 0); st != "TASK_STATUS_OK" {
		t.Fatalf("assert6: install-шаг (plan_index 0) при повторе status=%q, ожидался TASK_STATUS_OK (идемпотентно, changed=false)", st)
	}
	if st := taskStatusByPlan(t, stack, idemApply, sid, 1); st != "TASK_STATUS_OK" {
		t.Fatalf("assert6: потребитель (plan_index 1) при повторе status=%q, ожидался TASK_STATUS_OK", st)
	}
	stack.AssertTaskRegisterField(t, idemApply, sid, 1, "result", "PONG")
}

// distinctTaskCount — число РАЗНЫХ plan_index в task.executed прогона (= число
// исполненных задач). Больше числа авторских задач сценария означает впрыснутый
// синтез-шаг. Читает audit_log — единственную per-task persistence keeper-а.
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

// taskStatusByPlan — литерал TaskStatus задачи по (applyID, sid, plan_index),
// последняя по created_at (retry/cross-keeper дубль). plan_index глобально
// уникален сквозь Passage (миграция 079) → разрез по passage не нужен.
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
		t.Fatalf("taskStatusByPlan(apply=%s sid=%s plan_index=%d): нет task.executed-строки: %v", applyID, sid, planIdx, err)
	}
	return status
}

// taskErrorByPlan — error.code/module/message задачи по plan_index (fail-closed
// негатив). Пустые поля → маркеры "<no-error>"/"<no-module>"/"".
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
		t.Fatalf("taskErrorByPlan(apply=%s sid=%s plan_index=%d): нет task.executed-строки: %v", applyID, sid, planIdx, err)
	}
	return code, module, message
}

// assertHostFileExecutable — файл существует и имеет x-бит (host-слот бинаря
// soul-mod-redis, ADR-065(g)). Реюз assertHostFileAbsent недостаточно — нужен `test -x`.
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
		t.Fatalf("assertHostFileExecutable(%s): не исполняемый (test -x exit=%d)", path, code)
	}
}
