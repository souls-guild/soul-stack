//go:build e2e_live

// FC-2 L3b: changed_when + onchanges-каскад от РЕАЛЬНОГО changed-статуса на
// real-apply (idempotency).
//
// ЦЕЛЬ. L3a-stub (tests/e2e) отдаёт Changed:true/false из фикстуры — реальная
// идемпотентность core.file.rendered / core.service НЕ доказана, а onchanges-
// каскад может ложно срабатывать/не срабатывать на «фейковом» changed. Здесь
// каскад прогоняется через ПОДЛИННЫЙ Soul Apply в Debian-12-контейнере: SHA-256-
// сверка контента (core.file.rendered) и is-active/is-enabled-сверка
// (core.service) определяют register.changed, от которого зависит soul-side
// skipOnChanges (applyrunner.go::skipOnChanges).
//
// Сценарий — examples/service/smoke-nginx-live/create (тот же, что L3b-smoke):
//   plan_index 0: core.pkg.installed nginx
//   plan_index 1: core.file.rendered  /etc/nginx/sites-available/default
//                 register: nginx_default_conf  ← источник onchanges
//   plan_index 2: core.service.running nginx (enabled:true)
//   plan_index 3: core.service.restarted nginx  onchanges:[nginx_default_conf]  ← handler
//
// Все 4 задачи в ОДНОМ Passage (passage=0): smoke-nginx не использует
// where:/when: по register, поэтому keeper не стратифицирует план — onchanges
// источник и handler сидят в одном Passage и handler-skip решается soul-side
// (skipOnChanges), а не cross-passage keeper-логикой. N=1-прогон → plan_index ==
// позиция задачи в плане.
//
// ★ ЯДРО FC-2 — два прогона ОДНОГО create на ОДНУ incarnation, разные apply_id:
//   Прогон 1 (чистый контейнер): file changed=true → handler CHANGED → nginx
//       реально перезапущен.
//   Прогон 2 (тот же state, повтор): file changed=false (SHA совпал) → handler
//       onchanges НЕ срабатывает → TASK_STATUS_SKIPPED.
//
// Если прогон 2 даёт file changed=true на неизменном файле ИЛИ handler
// исполняется повторно (не SKIPPED) — это РЕАЛЬНЫЙ дефект идемпотентности
// core.file.rendered / onchanges-каскада, не артефакт теста.
//
// Helpers — FC-0 (AssertTaskStatus / AssertTaskRegisterField читают audit_log /
// apply_task_register по plan_index) + AssertHostServiceActive. Новых helpers в
// shared harness НЕ добавляем (FC-2-локальные ассерты — в этом файле).
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// plan_index задач create-сценария smoke-nginx-live (один Passage, N=1-прогон →
// plan_index == позиция). Источник правды — scenario/create/main.yml.
const (
	fc2PkgPlanIdx     = 0 // core.pkg.installed nginx
	fc2FilePlanIdx    = 1 // core.file.rendered (register: nginx_default_conf)
	fc2ServicePlanIdx = 2 // core.service.running nginx
	fc2HandlerPlanIdx = 3 // core.service.restarted (onchanges: [nginx_default_conf])
	fc2Passage        = 0 // smoke-nginx не стратифицируется — единственный Passage
)

func TestFC2OnchangesIdempotency(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/smoke-nginx-live",
		ServiceName: "smoke-nginx-live",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	sid := stack.SoulContainers[0].SID
	if sid != wantSID {
		t.Fatalf("SoulContainers[0].SID = %q, ожидалось %q", sid, wantSID)
	}

	const incName = "fc2-onchanges"
	const nginxConfPath = "/etc/nginx/sites-available/default"

	// Coven-членство ДО Create: roster резолвится по incarnation.name ∈ coven[]
	// (ADR-008). Без него scenario видит no_hosts → ноль apply_runs (как L3b-smoke).
	stack.AddSoulToCoven(t, 0, incName)

	// ── Прогон 1: чистый контейнер ───────────────────────────────────────────
	// POST /v1/incarnations авто-запускает create и возвращает его apply_id.
	inc, apply1 := stack.CreateIncarnationWithApply(t, incName, "smoke-nginx-live@main", map[string]any{
		"hostname": wantSID,
	})

	// 300 c — apt-get update + install nginx + systemctl на свежем Debian-12.
	stack.WaitApplySuccess(t, apply1, 300)
	stack.WaitIncarnationReady(t, inc, 30)

	// 1.1 file-задача реально changed=true (новый конфиг лёг на диск).
	stack.AssertTaskRegisterField(t, apply1, sid, fc2FilePlanIdx, "changed", "true")

	// 1.2 handler (core.service.restarted, onchanges) сработал → CHANGED.
	// restarted безусловно changed=true (service.go applyRestarted), а onchanges
	// его запустил, т.к. источник changed.
	stack.AssertTaskStatus(t, apply1, sid, fc2HandlerPlanIdx, fc2Passage, "TASK_STATUS_CHANGED")

	// 1.3 сервис реально активен (systemctl is-active внутри контейнера).
	stack.AssertHostServiceActive(t, 0, "nginx")

	// ── Прогон 2: тот же state, повтор того же create ─────────────────────────
	// RunScenario(create) на ready-incarnation проходит lock-gate (RunTyped
	// блокирует только error_locked, incarnation_typed.go) → новый apply_id.
	// Контейнер не трогаем между прогонами — конфиг идентичен.
	apply2 := stack.RunScenario(t, incName, "create", map[string]any{
		"hostname": wantSID,
	})
	stack.WaitApplySuccess(t, apply2, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// 2.1 ★ file-задача NOT changed — SHA-256 совпал, perm/owner не дрейфнули
	// (core.file.rendered: changed = contentChanged||modeChanged||ownerChanged).
	// Это и есть РЕАЛЬНАЯ идемпотентность core.file.rendered.
	assertFileNotChanged(t, stack, apply2, sid)

	// 2.2 ★ handler onchanges → SKIPPED. Источник не changed → soul-side
	// skipOnChanges=true → core.service.restarted НЕ исполняется. Ядро FC-2:
	// каскад не срабатывает повторно на неизменном файле.
	assertHandlerSkipped(t, stack, apply2, sid)

	// 2.3 sanity: pkg / service.running на повторе тоже не падают (idempotency
	// всей цепочки, не только handler). pkg: репо-latest стоит → OK; service.
	// running: active+enabled → OK. Допускаем OK или CHANGED у обоих (зависит от
	// того, дрейфнул ли репо/enabled между прогонами), но НЕ FAILED/SKIPPED.
	assertNotFailedNotSkipped(t, stack, apply2, sid, fc2PkgPlanIdx, "core.pkg.installed")
	assertNotFailedNotSkipped(t, stack, apply2, sid, fc2ServicePlanIdx, "core.service.running")

	// 2.4 сервис остался активен (handler skip не сломал сервис — он и не должен
	// был его трогать).
	stack.AssertHostServiceActive(t, 0, "nginx")

	t.Logf("FC-2 каскад доказан: прогон1 file.changed=true→handler CHANGED→nginx active; "+
		"прогон2 file.changed=false→handler SKIPPED (idempotency). apply1=%s apply2=%s",
		apply1, apply2)
}

// assertFileNotChanged — диагностический wrapper над FC-0 register-чтением:
// прогон 2 file-задачи ОБЯЗАН быть changed=false. Если changed=true на
// неизменном файле — это дефект идемпотентности core.file.rendered (ложный
// re-render), и сообщение должно это назвать прямо, а не «assert failed».
func assertFileNotChanged(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	got := registerChangedField(t, stack, applyID, sid, fc2FilePlanIdx)
	if got != "false" {
		t.Fatalf("★ FC-2 ДЕФЕКТ idempotency: прогон 2 core.file.rendered changed=%q, "+
			"ожидалось \"false\" (тот же контент/perm/owner на диске). Либо SHA-256-сверка "+
			"не идемпотентна, либо perm/owner дрейфят на каждом apply → onchanges-каскад "+
			"будет ложно срабатывать при каждом прогоне. apply=%s sid=%s plan_index=%d",
			got, applyID, sid, fc2FilePlanIdx)
	}
}

// assertHandlerSkipped — ядро FC-2: handler (onchanges по неизменному источнику)
// ОБЯЗАН быть TASK_STATUS_SKIPPED на прогоне 2. Если он CHANGED/OK — onchanges
// сработал на не-changed источнике → дефект каскада (handler исполнился, когда
// не должен; nginx перезапустился бы на каждом no-op apply).
func assertHandlerSkipped(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	got := taskStatusField(t, stack, applyID, sid, fc2HandlerPlanIdx, fc2Passage)
	if got != "TASK_STATUS_SKIPPED" {
		t.Fatalf("★ FC-2 ДЕФЕКТ каскада: прогон 2 handler (core.service.restarted, "+
			"onchanges:[nginx_default_conf]) status=%q, ожидался TASK_STATUS_SKIPPED. "+
			"Источник onchanges на прогоне 2 не changed — handler НЕ должен исполняться. "+
			"Срабатывание = ложный restart nginx на каждом no-op apply. apply=%s sid=%s plan_index=%d",
			got, applyID, sid, fc2HandlerPlanIdx)
	}
}

// assertNotFailedNotSkipped — sanity-страховка цепочки: задача на повторе не
// упала (FAILED/TIMED_OUT) и не была неожиданно пропущена (SKIPPED — у этих
// задач нет when:/onchanges:, SKIPPED означал бы сломанный gating). OK или
// CHANGED — оба валидны.
func assertNotFailedNotSkipped(t *testing.T, stack *harness.Stack, applyID, sid string, planIdx int, mod string) {
	t.Helper()
	got := taskStatusField(t, stack, applyID, sid, planIdx, fc2Passage)
	switch got {
	case "TASK_STATUS_OK", "TASK_STATUS_CHANGED":
		return
	default:
		t.Fatalf("FC-2 sanity: прогон 2 %s status=%q, ожидался OK/CHANGED "+
			"(задача без gating не должна падать/skip-аться на повторе). apply=%s sid=%s plan_index=%d",
			mod, got, applyID, sid, planIdx)
	}
}

// registerChangedField читает register_data->>'changed' задачи (apply_id, sid,
// plan_index) из apply_task_register напрямую — локальный аналог FC-0
// AssertTaskRegisterField, но возвращает значение для диагностического сравнения
// (FC-0-версия делает t.Fatal на мисматч, а нам нужно собственное сообщение про
// «дефект idempotency»). Источник тот же — миграция 079 apply_task_register.
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
		t.Fatalf("registerChangedField(apply=%s sid=%s plan_index=%d): нет register-строки "+
			"(реальный soul не вернул register?): %v", applyID, sid, planIdx, err)
	}
	return strings.TrimSpace(got)
}

// taskStatusField читает payload->>'status' задачи (apply_id, sid, plan_index,
// passage) из audit_log (event_type=task.executed) — локальный аналог FC-0
// AssertTaskStatus, возвращающий литерал для собственного сравнения. Берёт
// последнюю по created_at строку (та же «последняя побеждает», что в FC-0).
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
		t.Fatalf("taskStatusField(apply=%s sid=%s plan_index=%d passage=%d): нет "+
			"task.executed-строки (задача не исполнялась / TaskEvent не дошёл?): %v",
			applyID, sid, planIdx, passage, err)
	}
	return strings.TrimSpace(status)
}
