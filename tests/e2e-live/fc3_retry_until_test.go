//go:build e2e_live

// FC-3 L3b: retry:+until: health-gate против РЕАЛЬНОГО тайминга на real-apply.
//
// Дыра, которую закрывает тест. L3a-stub (tests/e2e/) отдаёт финальный RunResult
// сразу — retry-петля Soul-демона (runTaskWithRetry, applyrunner.go) НЕ входит, и
// не доказано, что until: вычисляется на реальном поднятии сервиса (а не
// сматчивается с 1-й попытки). Промежуточные попытки наружу НЕ эмитятся (контракт
// «один TaskEvent на task_idx»), поэтому ретраи невидимы через apply_runs /
// audit_log / register. Здесь — ПОДЛИННЫЙ soul-бинарь в Debian-12-контейнере,
// probe бьётся в детерминированный gate, физически не готовый на 1-й попытке.
//
// Сервис tests/e2e-live/fc3-retry-until (НЕ examples/** — WIP-зона). scenario
// create:
//   - Задача 0 (prepare): пишет на хост start-эпоху (/tmp/fc3-start) и ready-эпоху
//     В БУДУЩЕМ (now + gateDelaySec → /tmp/fc3-ready-at). Gate без фонового
//     процесса/гонки: «готов» = настенное время догнало ready-эпоху.
//   - Задача 1 (probe): core.cmd.shell с retry:{count:20, delay:2s,
//     until: register.self.stdout == 'READY'}. Пока now < ready-at → NOTYET
//     (until ложно → delay → retry); now >= ready-at → READY + записывает СВОЮ
//     done-эпоху в /tmp/fc3-done. 1-я попытка probe бежит сразу после prepare,
//     поэтому ОБЯЗАНА увидеть NOTYET → реальный retry.
//
// ASSERT (★ proof на real-apply):
//  1. probe в итоге OK (until сматчился), apply_runs success — until сошёлся на
//     реальном тайминге (не упал раньше, не исчерпал петлю).
//  2. ★ until реально прошёл ≥2 попытки — ДВА независимых способа:
//     (a) ТОЧНО: soul_apply_task_retries_total (counter повторов со 2-й попытки,
//         applyrunner.go runTaskWithRetry) >= 1, scrape container-side через
//         Exec(curl 127.0.0.1:9091/metrics). >=1 ⟹ попытка №2+ реально запущена.
//     (b) ТАЙМИНГ (corroboration): done-эпоха − start-эпоха >= gateDelaySec.
//         На 1-й попытке (мгновенно после prepare) разница была бы ~0; >= gate
//         ⟹ между prepare и success-probe прошёл реальный retry-бюджет.
//  3. ★ Дефекты: probe-register stdout != READY на success / done-эпоха
//     отсутствует / counter == 0 при success → реальный дефект retry:until
//     (until не ретраит / register.self не обновляется между попытками /
//     метрика не инкрементируется). Каждый — отдельный t.Fatal с диагнозом.
package e2e_live_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// gateDelaySec — задержка готовности health-gate (scenario prepare: ready-at =
// now + 6). probe-delay 2s × count 20 = 40s бюджет с запасом. 6s гарантирует, что
// 1-я (мгновенная) и 2-я (~+2s) попытки probe увидят NOTYET — until ОБЯЗАН
// ретраить. Должен совпадать с `+ 6` в scenario/create/main.yml::prepare.
const gateDelaySec = 6

func TestFC3RetryUntil_HealthGateOnRealTiming(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/fc3-retry-until",
		ServiceName: "fc3-retry-until",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if got := stack.SoulContainers[0].SID; got != wantSID {
		t.Fatalf("SoulContainers[0].SID = %q, ожидалось %q", got, wantSID)
	}

	const incName = "fc3-retry-until"

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → ноль строк apply_runs.
	stack.AddSoulToCoven(t, 0, incName)

	// POST /v1/incarnations авто-запускает scenario create и возвращает apply_id.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "fc3-retry-until@main", nil)

	// ── (1) probe в итоге OK, apply_runs success ─────────────────────────────
	// Бюджет: gate 6s + retry-петля до 40s + cold-start контейнера. 90s с запасом.
	// success ⟹ until сматчился (исчерпание дало бы FAILED flowcontrol.until_exhausted
	// → WaitApplySuccess зафейлил бы на terminal failed).
	stack.WaitApplySuccess(t, applyID, 90)
	stack.WaitIncarnationReady(t, inc, 30)

	// ── (2a) ★ ТОЧНО: soul_apply_task_retries_total >= 1 ─────────────────────
	// Counter инкрементируется runTaskWithRetry на КАЖДОЙ попытке со 2-й
	// (applyrunner.go: `if attempt > 1 { r.metrics.ObserveRetry() }`). >=1 ⟹
	// попытка №2 реально запущена → until не сматчился с 1-й. Scrape container-
	// side: метрик-порт на loopback (наружу не публикуется).
	retries := scrapeSoulMetricSum(t, stack, 0, "soul_apply_task_retries_total")
	if retries < 1 {
		t.Fatalf("★ ДЕФЕКТ retry:until: soul_apply_task_retries_total = %v, ожидалось >=1 — "+
			"probe сматчил until с 1-й попытки (gate не сработал / until не ретраит)", retries)
	}
	t.Logf("FC-3: soul_apply_task_retries_total = %v (>=1 → ретрай реально был)", retries)

	// ── (2b) ★ ТАЙМИНГ (corroboration): done − start >= gateDelaySec ──────────
	// probe записал done-эпоху на ветке READY; prepare — start-эпоху. Разница —
	// настенное время от prepare до success-попытки probe. >= gate ⟹ между ними
	// прошёл реальный retry-бюджет (на 1-й попытке было бы ~0).
	startEpoch := readHostEpochFile(t, stack, 0, "/tmp/fc3-start")
	doneEpoch := readHostEpochFile(t, stack, 0, "/tmp/fc3-done") // ДЕФЕКТ если файла нет: probe не дошёл до READY
	elapsed := doneEpoch - startEpoch
	if elapsed < gateDelaySec {
		t.Fatalf("★ ДЕФЕКТ retry:until: done−start = %d c, ожидалось >= %d c — "+
			"probe сматчил READY раньше открытия gate (until вычислился по СТАРОМУ register / gate-логика сломана)",
			elapsed, gateDelaySec)
	}
	t.Logf("FC-3: done−start = %d c (>= gate %d c → until сошёлся на реальном тайминге)", elapsed, gateDelaySec)

	// ── (3) ★ Дефект-страховка: register.self.stdout probe == READY на success ─
	// register persisted keeper-ом из TaskEvent.register_data ПОСЛЕДНЕЙ попытки
	// (контракт runTaskWithRetry: наружу эмитится финальная попытка). На success
	// финальная попытка ДОЛЖНА быть READY — иначе until сматчился бы на не-READY
	// (register.self не обновлялся между попытками — реальный дефект).
	// plan_index probe = 1 (задача 0 prepare, задача 1 probe; один Passage).
	stack.AssertTaskRegisterField(t, applyID, wantSID, 1, "stdout", "READY")
}

// scrapeSoulMetricSum читает сумму prometheus-метрики soul_*-listener-а ВНУТРИ
// soul-контейнера через Exec(curl loopback). Soul-метрик-порт (127.0.0.1:9091,
// metrics.enabled в harness soul.yml) наружу не публикуется — scrape только
// container-side. Парсер — простой суммирующий греп по имени метрики (counter без
// label-ов → одна строка `<name> <value>`); HELP/TYPE-строки (`# `) пропускаются.
//
// Свой helper (не shared harness.AssertMetricGE): тот скрейпит keeper-метрики по
// HTTP с хоста (Stack.MetricsURL), а здесь — soul-метрики из контейнера.
func scrapeSoulMetricSum(t *testing.T, stack *harness.Stack, soulIdx int, metric string) float64 {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{
		"curl", "-fsS", "http://127.0.0.1:9091/metrics",
	})
	if err != nil || code != 0 {
		t.Fatalf("scrapeSoulMetricSum(%s): curl /metrics: code=%d err=%v out=%s", metric, code, err, out)
	}

	var sum float64
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Counter без label-ов: `soul_apply_task_retries_total 3`. С label-ами
		// было бы `name{...} v` — startsWith по имени + следующий символ ' '/'{'.
		if !strings.HasPrefix(line, metric) {
			continue
		}
		rest := line[len(metric):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '{') {
			continue // другая метрика с тем же префиксом (..._total vs ..._total_sum)
		}
		fields := strings.Fields(line)
		v, perr := strconv.ParseFloat(fields[len(fields)-1], 64)
		if perr != nil {
			t.Fatalf("scrapeSoulMetricSum(%s): parse value %q: %v", metric, fields[len(fields)-1], perr)
		}
		sum += v
		found = true
	}
	if !found {
		t.Fatalf("scrapeSoulMetricSum(%s): метрика не найдена в soul /metrics:\n%s", metric, out)
	}
	return sum
}

// readHostEpochFile читает unix-эпоху (целое) из файла на ХОСТЕ soul-контейнера
// через Exec(cat). Файл отсутствует / нечисловой → t.Fatal с диагнозом (для
// /tmp/fc3-done отсутствие = probe не дошёл до READY — реальный дефект retry:until).
func readHostEpochFile(t *testing.T, stack *harness.Stack, soulIdx int, path string) int64 {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, code, err := sc.Exec(ctx, []string{"cat", path})
	if err != nil {
		t.Fatalf("readHostEpochFile(%s): exec: %v out=%s", path, err, out)
	}
	if code != 0 {
		t.Fatalf("★ readHostEpochFile(%s): файла нет (exit=%d) — probe не записал метку "+
			"(не дошёл до READY-ветки / gate-задача не отработала)", path, code)
	}
	epoch, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		t.Fatalf("readHostEpochFile(%s): нечисловое содержимое %q: %v", path, strings.TrimSpace(out), perr)
	}
	return epoch
}
