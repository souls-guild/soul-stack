//go:build e2e_live

// L3b E2E staged-render capstone (ADR-056 §S5). S3 доказал staged probe→where на
// soul-STUB (scripted RunResult); этот тест — на ПОДЛИННОМ soul-бинаре в реальном
// флоте из двух Debian-12-контейнеров.
//
// Service tests/e2e-live/staged-probe-live (НЕ examples/** — WIP-зона пользователя):
// scenario create стратифицируется keeper-ом в N=2 Passage по register-зависимости:
//   - Passage 0: probe роли каждого хоста (core.cmd.shell printf master|slave по
//     hostname) → register: role. РЕАЛЬНЫЙ soul исполняет команду и эмитит
//     TaskEvent.register_data; keeper собирает per-host register в apply_task_register.
//   - Passage 1: core.file.present создаёт маркер /tmp/acted-on-master ТОЛЬКО там,
//     где register.role.stdout == 'master' — keeper резолвит where СВЕЖИМ per-host
//     register, собранным барьером Passage 0.
//
// ASSERT (★ capstone proof):
//  1. apply_runs success на обоих хостах (staged-прогон сошёлся end-to-end).
//  2. register-сбор: apply_task_register несёт stdout=master для soul-live-a и
//     stdout=slave для soul-live-b — реальный soul вернул register, keeper собрал.
//  3. passage-1 targeting: маркер ЕСТЬ на master-хосте (soul-live-a) и ОТСУТСТВУЕТ
//     на slave-хосте (soul-live-b) — Passage-1 ApplyRequest пришёл ТОЛЬКО master-у.
//  4. host_state apply_runs: у soul-live-a две passage-строки (0 и 1), у soul-live-b
//     passage-1 строка либо no_match, либо отсутствует (where отфильтровал slave).
package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bStagedProbeLive_WhereTargetsOnlyMaster(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e-live/staged-probe-live",
		ServiceName: "staged-probe-live",
		Souls:       2,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 2 {
		t.Fatalf("ожидалось 2 soul-контейнера, получено %d", got)
	}
	const (
		masterSID = "soul-live-a.example.com" // probe printf master
		slaveSID  = "soul-live-b.example.com" // probe printf slave
	)
	if got := stack.SoulContainers[0].SID; got != masterSID {
		t.Fatalf("SoulContainers[0].SID = %q, ожидалось %q", got, masterSID)
	}
	if got := stack.SoulContainers[1].SID; got != slaveSID {
		t.Fatalf("SoulContainers[1].SID = %q, ожидалось %q", got, slaveSID)
	}

	const incName = "test-staged-probe"

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → ноль строк apply_runs.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
	}

	// POST /v1/incarnations авто-запускает scenario create (= staged-сценарий) и
	// возвращает его apply_id. На обоих хостах passage-capability анонсирована
	// (один бета-бинарь), поэтому forward-compat-гейт пропускает прогон.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "staged-probe-live@main", nil)

	// probe (echo роли) быстрый; staged-loop добавляет один barrier. 120 c с
	// запасом на cold-start контейнеров и render двух Passage.
	stack.WaitApplySuccess(t, applyID, 120)
	stack.WaitIncarnationReady(t, inc, 30)

	// ── (2) register-сбор у slave (probe register выжил) ────────────────────
	// probe (Passage 0) — task_idx 0 в ApplyRequest Passage 0. register_data->>
	// 'stdout' — то, что РЕАЛЬНЫЙ soul напечатал и эмитил в TaskEvent.register_data.
	//
	// Читаем register ТОЛЬКО slave: его probe-роль (slave) НЕ прошла where Passage 1,
	// поэтому slave не исполнял действие Passage 1 → его probe register passage 0
	// (task_idx 0) не был перезаписан и читаем post-run. У master probe register
	// passage 0 ЗАТЁРТ действием Passage 1 (task_idx-коллизия — находка ниже), читать
	// его post-run нельзя. master доказывается targeting-ом (3): where не отобрал бы
	// master без корректного probe register, собранного keeper-ом.
	//
	// НАХОДКА (флаг в отчёте): apply_task_register PK = (apply_id, sid, task_idx) БЕЗ
	// passage, а soul эмитит task_idx = ПОЗИЦИЯ задачи в ApplyRequest СВОЕГО Passage
	// (не глобальный план-Index). probe (P0, поз.0) и действие (P1, поз.0) делят
	// task_idx=0 → ON CONFLICT перезаписывает probe register действием.
	assertRegisterStdout(t, stack, applyID, slaveSID, "slave")

	// ── (3) passage-1 targeting: маркер ТОЛЬКО на master ────────────────────
	// Это и есть транзитивное доказательство, что реальный soul вернул probe
	// register И keeper его собрал: without корректного per-host register Passage 0,
	// where: register.role.stdout=='master' Passage 1 не отобрал бы master.
	const marker = "/tmp/acted-on-master"
	stack.AssertHostFileExists(t, 0, marker) // soul-live-a (master) — есть.
	stack.AssertHostFileContent(t, 0, marker, "passage-1 ran here")
	assertHostFileAbsent(t, stack, 1, marker) // soul-live-b (slave) — НЕТ.

	// ── (4) apply_runs per-passage: master нёс Passage 0 и 1, slave — без P1 ─
	// where отфильтровал slave из Passage-1 таргета: либо нет строки passage=1,
	// либо она no_match. master имеет обе passage-строки success.
	assertPassageStatuses(t, stack, applyID, masterSID, map[int]string{0: "success", 1: "success"})
	assertSlaveNoPassage1Apply(t, stack, applyID, slaveSID)
}

// assertRegisterStdout проверяет, что keeper собрал register probe-задачи
// Passage 0 (task_idx 0) хоста sid со значением stdout=want — доказательство, что
// реальный soul вернул TaskEvent.register_data, keeper его персистил. Разрез по
// passage=0 обязателен (см. находку про task_idx-коллизию в основном тесте).
func assertRegisterStdout(t *testing.T, stack *harness.Stack, applyID, sid, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout string
	err := stack.DB().QueryRow(ctx,
		`SELECT COALESCE(register_data->>'stdout','<null>') FROM apply_task_register
		 WHERE apply_id = $1 AND sid = $2 AND task_idx = 0 AND passage = 0`, applyID, sid).Scan(&stdout)
	if err != nil {
		t.Fatalf("assertRegisterStdout(%s): нет register probe-задачи passage=0 (реальный soul не вернул register?): %v", sid, err)
	}
	if stdout != want {
		t.Fatalf("assertRegisterStdout(%s): register stdout = %q, ожидалось %q", sid, stdout, want)
	}
}

// assertHostFileAbsent — отрицательная host-проверка: файла НЕТ (stat exit != 0).
// Доказывает, что Passage-1 ApplyRequest НЕ пришёл этому хосту (where его отфильтровал).
func assertHostFileAbsent(t *testing.T, stack *harness.Stack, soulIdx int, path string) {
	t.Helper()
	sc := stack.SoulContainers[soulIdx]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := sc.Exec(ctx, []string{"stat", "-c", "%F", path})
	if err != nil {
		t.Fatalf("assertHostFileAbsent(soulIdx=%d path=%s): exec: %v\noutput=%s", soulIdx, path, err, out)
	}
	if code == 0 {
		t.Fatalf("★ assertHostFileAbsent(soulIdx=%d path=%s): файл СУЩЕСТВУЕТ — Passage-1 затаргетил slave, where не сработал (silent-wrong-target)", soulIdx, path)
	}
}

// assertPassageStatuses проверяет статус apply_runs-строки хоста по каждому
// ожидаемому passage. Доказывает, что master получил И probe (Passage 0), И
// действие (Passage 1) — обе строки success.
func assertPassageStatuses(t *testing.T, stack *harness.Stack, applyID, sid string, want map[int]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := stack.DB().Query(ctx,
		`SELECT passage, status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid)
	if err != nil {
		t.Fatalf("assertPassageStatuses(%s): query: %v", sid, err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var p int
		var status string
		if err := rows.Scan(&p, &status); err != nil {
			t.Fatalf("assertPassageStatuses(%s): scan: %v", sid, err)
		}
		got[p] = status
	}
	for p, ws := range want {
		gs, ok := got[p]
		if !ok {
			t.Fatalf("assertPassageStatuses(%s): нет apply_runs-строки passage=%d (got %v)", sid, p, got)
		}
		if gs != ws {
			t.Fatalf("assertPassageStatuses(%s): passage=%d status=%q, ожидалось %q", sid, p, gs, ws)
		}
	}
}

// assertSlaveNoPassage1Apply — slave НЕ исполнял Passage-1 действие: where:
// register.role.stdout=='master' отфильтровал его. Допустимо: либо нет строки
// passage=1 вовсе, либо она в no_match (where отобрал 0 хостов для slave). Строка
// passage=0 (probe) у slave должна быть success.
func assertSlaveNoPassage1Apply(t *testing.T, stack *harness.Stack, applyID, sid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := stack.DB().Query(ctx,
		`SELECT passage, status FROM apply_runs WHERE apply_id = $1 AND sid = $2`, applyID, sid)
	if err != nil {
		t.Fatalf("assertSlaveNoPassage1Apply(%s): query: %v", sid, err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var p int
		var status string
		if err := rows.Scan(&p, &status); err != nil {
			t.Fatalf("assertSlaveNoPassage1Apply(%s): scan: %v", sid, err)
		}
		got[p] = status
	}
	if got[0] != "success" {
		t.Fatalf("assertSlaveNoPassage1Apply(%s): probe passage=0 status=%q, ожидалось success", sid, got[0])
	}
	// Passage-1 строки либо нет (where отфильтровал slave из таргета), либо
	// no_match. success/changed на passage=1 означал бы, что действие выполнилось
	// на slave — silent-wrong-target.
	if p1, ok := got[1]; ok && p1 != "no_match" {
		t.Fatalf("★ assertSlaveNoPassage1Apply(%s): passage=1 status=%q — действие исполнилось на slave (where не отфильтровал, silent-wrong-target)", sid, p1)
	}
}
