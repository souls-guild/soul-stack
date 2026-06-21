//go:build e2e_live

// FC-4 L3b: failed_when:false = ignore_errors на РЕАЛЬНОМ падении модуля при
// real-apply. L3a-stub не вычисляет failed_when → best-effort-семантика
// (ignore_errors) и mapping реального провала в register.<name>.ignored_error
// там НЕ доказаны. Этот тест гоняет ПОДЛИННЫЙ soul-бинарь в Debian-12-контейнере.
//
// Service tests/e2e-live/fc4-ignore-errors-live (НЕ examples/** — WIP-зона):
// core.exec.run несуществующего бинаря РЕАЛЬНО падает на Soul-е (fork/exec: file
// not found → util.SendFailed → last.failed=true). Два сценария-зеркала:
//   - create:    падающая задача + failed_when:false → прогон SUCCESS, исходная
//                ошибка уходит в register.fail_probe.ignored_error (НЕ теряется).
//   - fail_hard: ТА ЖЕ задача БЕЗ failed_when → прогон FAILED → error_locked.
//
// ASSERT (★ ignore_errors proof на real-apply):
//  1. create: apply_runs success на хосте (failed_when:false подавил провал).
//  2. ★ register.fail_probe.ignored_error персистится с РЕАЛЬНОЙ ошибкой модуля
//     (непусто + содержит сигнатуру exec-провала). Имя поля сверено по
//     soul/internal/runtime/applyrunner.go (ev.RegisterData["ignored_error"],
//     ignore_errors-аудит, ~:959). register.failed == false (провал подавлен).
//  3. ★ Контраст: fail_hard (та же задача без failed_when) → прогон FAILED,
//     incarnation error_locked. Доказывает, что success в (1) — заслуга именно
//     failed_when:false, а не «модуль не падает».
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
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const sid = "soul-live-a.example.com"
	if got := stack.SoulContainers[0].SID; got != sid {
		t.Fatalf("SoulContainers[0].SID = %q, ожидалось %q", got, sid)
	}

	const incName = "test-fc4-ignore-errors"

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → ноль строк apply_runs.
	stack.AddSoulToCoven(t, 0, incName)

	// ── (1) create: реальный провал модуля + failed_when:false → SUCCESS ─────
	// POST /v1/incarnations авто-запускает create-scenario. На single-host задача
	// core.exec.run падает (binary not found), failed_when "false" перекрывает
	// провал → задача OK → прогон success.
	inc, createApplyID := stack.CreateIncarnationWithApply(t, incName, "fc4-ignore-errors-live@main", nil)

	// 120 c с запасом на cold-start контейнера (сама задача мгновенная — exec
	// падает на старте процесса, никакой apt/network).
	stack.WaitApplySuccess(t, createApplyID, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// ★ apply_runs хоста = success: failed_when:false подавил реальный провал.
	stack.AssertApplyHostStatus(t, createApplyID, sid, "success")

	// ── (2) ★ register.fail_probe.ignored_error несёт РЕАЛЬНУЮ ошибку ────────
	// Имя поля — ignored_error (applyrunner.go ignore_errors-аудит): при
	// !failed && moduleErr != nil исходная ошибка кладётся в register_data.
	// Единственная задача плана → plan_index=0. Точное значение — текст
	// exec-провала Go runtime (хрупко для ==), поэтому проверяем непусто +
	// сигнатуру.
	const planIdx = 0
	assertRegisterFieldContains(t, stack, createApplyID, sid, planIdx, "ignored_error",
		"/nonexistent/fc4-deliberate-fail")

	// failed=false в register — провал подавлен, итоговый flow-control-исход OK.
	// Это точное значение, безопасно для ==.
	stack.AssertTaskRegisterField(t, createApplyID, sid, planIdx, "failed", "false")

	// ── (3) ★ Контраст: fail_hard (та же задача без failed_when) → FAILED ────
	// Инкарнация в ready после create. Запускаем fail_hard — та же падающая
	// задача, но без failed_when:false. Реальный провал НЕ подавлен → прогон
	// FAILED → incarnation error_locked. Без этого контраста success в (1) можно
	// было бы списать на «модуль не падает» — а он падает (доказано здесь).
	failApplyID := stack.RunScenario(t, inc, "fail_hard", nil)

	// fail_hard оставляет incarnation в error_locked (run.go §7: state_changes не
	// коммитятся при terminal-failed барьере). WaitIncarnationStatus фейлит, если
	// достигнут любой ДРУГОЙ терминал (в т.ч. ready — это была бы регрессия
	// ignore_errors: провал НЕ должен подавляться без флага).
	stack.WaitIncarnationStatus(t, inc, "error_locked", 120)

	// ★ apply_runs хоста fail_hard = failed: тот же модуль, та же ошибка, но без
	// failed_when:false прогон валится. Контраст с (1) доказывает, что подавление
	// делает именно failed_when, а не сам модуль.
	stack.AssertApplyHostStatus(t, failApplyID, sid, "failed")
}

// assertRegisterFieldContains — register_data->>field хоста (plan_index)
// непуст и содержит want-подстроку. AssertTaskRegisterField делает точное ==,
// но ignored_error = текст exec-провала Go runtime (зависит от версии/ОС),
// поэтому для него нужна проверка «непусто + сигнатура», не точное равенство.
// Доказывает, что реальный stderr/диагностика провала реально доехала в
// register, а не пустая заглушка.
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
		t.Fatalf("assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): нет register-строки (задача без register:/реальный soul не вернул register?): %v",
			applyID, sid, planIdx, field, err)
	}
	if got == "" || got == "<null>" {
		t.Fatalf("★ assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): поле ПУСТО (%q) — ignored_error не персистнут / реальный провал не доехал в register",
			applyID, sid, planIdx, field, got)
	}
	if !strings.Contains(got, want) {
		t.Fatalf("★ assertRegisterFieldContains(apply=%s sid=%s plan_index=%d field=%s): %q НЕ содержит %q — register несёт не ту ошибку",
			applyID, sid, planIdx, field, got, want)
	}
}
