//go:build e2e_live

// L3b-6 E2E: Scry / check-drift на РЕАЛЬНОМ soul (ADR-031 Slice B).
//
// Закрывает единственный пробел drift-цели Фазы 4 clean-room: L3a
// (tests/e2e/drift_test.go) гоняет drift через stub-Plan (stub.SetDryRunPlan),
// реальный SoulModule.Plan core-модуля НЕ вызывается. Здесь — наоборот: Keeper
// рендерит scenario/converge, рассылает ApplyRequest{dry_run:true} реальному
// soul-контейнеру, Soul зовёт core.file.Plan (PlanReadSafe, pure-read) и
// сравнивает желаемый content vs фактический файл на хосте.
//
// Сервис — hello-world (core.file.present greeting-файл /tmp/soul-stack-hello),
// лёгкий: ~60s против ~5мин у nginx (нет apt-install). Это даёт ещё и clean-room
// parity getting-started-пути (тот же сервис, что в quickstart-доке).
//
// Flow:
//  1. NewStack + 1 real soul (Debian-12 systemd-PID-1).
//  2. apply create (input.greeting) → WaitApplySuccess → файл на хосте.
//  3. AssertHostFileExists/Content — реальный результат apply на хосте.
//  4. MUTATE через container.Exec: переписать файл чужим содержимым.
//  5. CheckDrift(greeting=<тот же>) → drifted=true (real Plan увидел расхождение).
//  6. re-apply create → CheckDrift → drifted=false (файл восстановлен, clean).
//
// Почему ловит регрессии, которые L3a не ловит:
//   - реальный core.file.Plan регрессировал (например, Plan(present) перестал
//     сравнивать content) → drifted=false после mutate → тест падает;
//   - DryRun не доходит до soul-контейнера на wire → Soul делает Apply вместо
//     Plan → мутирует файл, drift-ветка не сработает;
//   - converge-render/dispatch/driftBarrier сломались на реальном roster-е.
package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bDriftLive_HelloWorld(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-hello-world",
		ServiceName: "hello-world",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}

	const (
		greetingFile = "/tmp/soul-stack-hello"
		greeting     = "hello from soul-stack"
	)

	inc := stack.CreateIncarnation(t, "test-hello-drift", "hello-world@main", map[string]any{
		"greeting": greeting,
	})

	applyID := stack.RunScenario(t, inc, "create", map[string]any{
		"greeting": greeting,
	})
	// hello-world — без apt: render → core.file.present → RunResult быстрый.
	stack.WaitApplySuccess(t, applyID, 60)

	// Реальный результат apply на хосте: файл создан с переданным content.
	stack.AssertHostFileExists(t, 0, greetingFile)
	stack.AssertHostFileContent(t, 0, greetingFile, greeting)

	// state-commit зафиксировал путь (как L3a, но через реальный RunResult).
	stack.AssertIncarnationState(t, "test-hello-drift", map[string]any{
		"greeting_file": greetingFile,
	})

	// Sanity: clean baseline до мутации — converge видит совпадение, drift нет.
	// Это отделяет «Plan честно работает» от «Plan всегда видит drift».
	baseline := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	assertSingleHostStatus(t, baseline, stack.SoulContainers[0].SID, "clean")
	if baseline.Summary.HostsClean != 1 || baseline.Summary.HostsDrifted != 0 {
		t.Fatalf("baseline CheckDrift: ожидался clean=1 drifted=0; summary=%+v", baseline.Summary)
	}

	// MUTATE: переписываем greeting-файл чужим содержимым прямо в контейнере.
	// core.file.Plan(present) при следующем check-drift сравнит desired content
	// (greeting) vs фактический ("tampered") и вернёт changed=true.
	mutateHostFile(t, stack, 0, greetingFile, "tampered out-of-band\n")

	// CheckDrift → drifted: РЕАЛЬНЫЙ core.file.Plan увидел расхождение content.
	drifted := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	if drifted.CheckedAt.IsZero() {
		t.Fatalf("drifted DriftReport.checked_at пуст: %+v", drifted)
	}
	if drifted.ScenarioRef != "converge" {
		t.Fatalf("drifted DriftReport.scenario_ref=%q, ожидался converge", drifted.ScenarioRef)
	}
	h := assertSingleHostStatus(t, drifted, stack.SoulContainers[0].SID, "drifted")
	gotChanged := false
	for _, tk := range h.Tasks {
		if tk.Changed {
			gotChanged = true
			if tk.Module != "core.file.present" {
				t.Fatalf("drifted task module=%q, ожидался core.file.present: %+v", tk.Module, tk)
			}
		}
	}
	if !gotChanged {
		t.Fatalf("drifted DriftReport: ни одной changed-задачи (Plan не увидел drift?); tasks=%+v", h.Tasks)
	}
	if drifted.Summary.HostsDrifted != 1 || drifted.Summary.HostsClean != 0 {
		t.Fatalf("drifted CheckDrift: ожидался drifted=1 clean=0; summary=%+v", drifted.Summary)
	}

	// re-apply create — восстанавливает файл до desired content.
	reApplyID := stack.RunScenario(t, inc, "create", map[string]any{
		"greeting": greeting,
	})
	stack.WaitApplySuccess(t, reApplyID, 60)
	stack.AssertHostFileContent(t, 0, greetingFile, greeting)

	// CheckDrift → clean: файл восстановлен, реальный Plan не видит расхождения.
	clean := stack.CheckDrift(t, "test-hello-drift", map[string]any{"greeting": greeting})
	hc := assertSingleHostStatus(t, clean, stack.SoulContainers[0].SID, "clean")
	for _, tk := range hc.Tasks {
		if tk.Changed {
			t.Fatalf("clean DriftReport: changed-задача после re-apply: %+v", tk)
		}
	}
	if clean.Summary.HostsClean != 1 || clean.Summary.HostsDrifted != 0 {
		t.Fatalf("clean CheckDrift: ожидался clean=1 drifted=0; summary=%+v", clean.Summary)
	}
}

// assertSingleHostStatus проверяет, что в отчёте ровно один хост с ожидаемым
// SID и статусом, и возвращает его для дальнейшего разбора tasks.
func assertSingleHostStatus(t *testing.T, rep harness.DriftReport, wantSID, wantStatus string) harness.DriftHostReport {
	t.Helper()
	if len(rep.Hosts) != 1 {
		t.Fatalf("DriftReport.hosts: len=%d, ожидался 1; hosts=%+v", len(rep.Hosts), rep.Hosts)
	}
	h := rep.Hosts[0]
	if h.SID != wantSID {
		t.Fatalf("DriftReport.hosts[0].sid=%q, ожидался %q", h.SID, wantSID)
	}
	if h.Status != wantStatus {
		t.Fatalf("DriftReport.hosts[0].status=%q, ожидался %q; host=%+v", h.Status, wantStatus, h)
	}
	return h
}

// mutateHostFile переписывает файл внутри soul-контейнера out-of-band (минуя
// scenario-apply) — имитация drift-а, который Scry-проверка обязана заметить.
func mutateHostFile(t *testing.T, s *harness.Stack, soulIdx int, path, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sc := s.SoulContainers[soulIdx]
	out, code, err := sc.Exec(ctx, []string{
		"/bin/sh", "-c", "printf %s " + shellSingleQuote(content) + " > " + shellSingleQuote(path),
	})
	if err != nil || code != 0 {
		t.Fatalf("mutateHostFile(%s): exec code=%d err=%v output=%s", path, code, err, out)
	}
}

// shellSingleQuote — POSIX single-quote-экранирование для безопасной подстановки
// тест-литералов в `/bin/sh -c` (path/content из теста, не user-input).
func shellSingleQuote(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	return out + "'"
}
