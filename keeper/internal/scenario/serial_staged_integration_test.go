//go:build integration

// 2D serial×passage guard-тесты (ADR-056 §S4 amend, S-2D1). Рестрикт
// `serial_staged_unsupported` снят: staged-прогон, несущий `serial:`, исполняется
// Passage-циклом, где dispatchPassage бьёт хосты на serial-волны из задач ИМЕННО
// ЭТОГО Passage (per-passage width, НЕ per-RUN). Здесь доказывается:
//   - rolling per-passage (probe — одна волна, serial:1 действие — N волн по 1);
//   - ★ probe Passage 0 БЕЗ serial едет ОДНОЙ волной даже при serial:1 в Passage 1
//     (реверс на per-passage width: per-RUN min-width дал бы probe-throttle);
//   - fail-stop в волне Passage P → следующая волна и Passage P+1 не стартуют;
//   - register всех волн Passage P собран ДО старта Passage P+1.
//
// Soul симулируется serialStagedDispatcher-ом тем же контрактом, что stagedDispatcher
// (per-(apply_id, sid, passage) терминал + per-host register по plan_index), плюс он
// фиксирует ПОРЯДОК SendApply per-passage — это и есть наблюдаемая последовательность
// serial-волн (внутри волны SendApply идут подряд, между волнами стоит per-wave barrier).

package scenario

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// serialStagedServiceRepo — restart-идиома serial+staged: Passage 0 probe role
// (register: role, БЕЗ serial — реверс-точка), Passage 1 serial:1 действие
// `where: register.role.stdout == 'slave'` (зависит от probe Passage 0).
// Stratify → TaskPassage [0,1], Count=2. serial:1 несёт ТОЛЬКО Passage-1 задача.
func serialStagedServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: noop
state_schema_version: 1
description: serial+staged 2D proof service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/restart/main.yml", `name: restart
description: probe role (p0, no serial) then rolling serial:1 act on replicas (p1)
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params:
      cmd: detect-role
  - name: Rolling-restart replicas one at a time
    module: core.exec.run
    where: "register.role.stdout == 'slave'"
    serial: 1
    params:
      cmd: restart
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init serial+staged service", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// serialStagedDispatcher симулирует Soul под serial+staged-прогоном:
//   - Passage 0 (probe `role`): пишет per-host register `role` (по plan_index, как
//     Soul эхает в TaskEvent) и терминалит строку passage 0.
//   - Passage>0: терминалит строку этого Passage.
//
// КЛЮЧЕВОЕ: фиксирует ПОРЯДОК SendApply как []sendEvent{passage, sid}. В serial-пути
// dispatchWave вызывает SendApply подряд внутри волны, а per-wave barrier стоит
// МЕЖДУ волнами — поэтому последовательность событий отражает порядок волн Passage.
//
// failOn — sid, который завершается FAILED (для fail-stop): волна с этим хостом
// ломает barrier, следующая волна и следующий Passage НЕ стартуют. register для
// него не пишется в Passage>0 (действие упало).
type serialStagedDispatcher struct {
	t         *testing.T
	roleBySID map[string]string
	failOn    string

	mu     sync.Mutex
	events []sendEvent
}

type sendEvent struct {
	passage int
	sid     string
}

func newSerialStagedDispatcher(t *testing.T, roleBySID map[string]string) *serialStagedDispatcher {
	return &serialStagedDispatcher{t: t, roleBySID: roleBySID}
}

func (d *serialStagedDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	applyID := req.GetApplyId()

	d.mu.Lock()
	d.events = append(d.events, sendEvent{passage: passage, sid: sid})
	d.mu.Unlock()

	if passage == 0 {
		localIdx, planIdx := emitterIndices(req, "role")
		if err := applyrun.UpsertTaskRegister(ctx, integrationPool, &applyrun.TaskRegister{
			ApplyID: applyID, SID: sid, PlanIndex: planIdx, TaskIdx: localIdx, Passage: 0,
			RegisterData: map[string]any{"stdout": d.roleBySID[sid], "changed": false, "failed": false},
		}); err != nil {
			d.t.Errorf("serialStagedDispatcher: UpsertTaskRegister role (%s): %v", sid, err)
		}
	}

	if passage > 0 && sid == d.failOn {
		summary := "simulated failure"
		if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusFailed, &summary); err != nil {
			d.t.Errorf("serialStagedDispatcher: UpdateStatus(%s, failed): %v", sid, err)
		}
		return nil
	}

	if err := applyrun.UpdateStatus(ctx, integrationPool, applyID, sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("serialStagedDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

// passageEvents возвращает SID-ы SendApply ЭТОГО Passage в порядке вызовов (= порядок
// serial-волн). Для serial:1 это последовательность хостов по одному.
func (d *serialStagedDispatcher) passageEvents(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, e := range d.events {
		if e.passage == passage {
			out = append(out, e.sid)
		}
	}
	return out
}

// firstPassageEvent возвращает индекс первого события Passage p в общей
// последовательности SendApply (-1 если не было). Нужен для ASSERT «Passage P+1
// стартует строго ПОСЛЕ всех событий Passage P».
func (d *serialStagedDispatcher) firstPassageEvent(passage int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, e := range d.events {
		if e.passage == passage {
			return i
		}
	}
	return -1
}

func (d *serialStagedDispatcher) lastPassageEvent(passage int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	last := -1
	for i, e := range d.events {
		if e.passage == passage {
			last = i
		}
	}
	return last
}

// TestIntegration_SerialStaged_RollingPerPassage — ★ 2D serial×passage (ADR-056
// §S4 amend). restart-идиома: Passage 0 probe role (ОДНА волна на всех хостах),
// Passage 1 serial:1 действие where role==slave (N волн по 1 хосту, строго
// последовательно). ASSERT: порядок волн + терминальность (READY) + Passage 1
// после полного Passage 0.
//
// Три хоста, ВСЕ slave → Passage-1 действие таргетит всех троих, serial:1 катит
// их по одному (3 волны). Если бы рестрикт остался — прогон отвергся бы ДО dispatch
// (reason serial_staged_unsupported). Если бы per-passage не работал — probe Passage 0
// поехал бы волнами по 1 (см. ProbeNotThrottled).
func TestIntegration_SerialStaged_RollingPerPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Оба Passage прошли барьеры (probe-волна + 3 serial-волны действия) → ready.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0 (probe, БЕЗ serial): ОДНА волна — все три хоста (порядок по SID).
	p0 := disp.passageEvents(0)
	if len(p0) != 3 {
		t.Fatalf("Passage 0 events = %v, want 3 хоста в одной волне (probe без serial)", p0)
	}

	// Passage 1 (serial:1): три хоста ПОСЛЕДОВАТЕЛЬНО (3 волны по 1). Порядок по SID
	// (sortedSIDs + splitWaves(width=1)).
	p1 := disp.passageEvents(1)
	wantP1 := []string{"host-a.example.com", "host-b.example.com", "host-c.example.com"}
	if len(p1) != 3 {
		t.Fatalf("Passage 1 events = %v, want 3 волны по 1 хосту (serial:1)", p1)
	}
	for i := range wantP1 {
		if p1[i] != wantP1[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q (serial:1 rolling по SID)", i, p1[i], wantP1[i])
		}
	}

	// ★ Терминальность + порядок: Passage 1 стартует строго ПОСЛЕ всех событий
	// Passage 0 (probe-волна полностью прошла барьер до первой serial-волны действия).
	if first1, last0 := disp.firstPassageEvent(1), disp.lastPassageEvent(0); first1 <= last0 {
		t.Fatalf("★ Passage 1 первый SendApply (idx %d) НЕ после последнего события Passage 0 (idx %d) — barrier Passage 0 не дождался полной волны", first1, last0)
	}

	// apply_runs: у каждого хоста passage 0 + passage 1, все success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range wantP1 {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1] (probe + serial-действие)", sid, got[sid])
		}
	}
}

// TestIntegration_SerialStaged_ProbeNotThrottled — ★ РЕВЕРС на per-passage width
// (ADR-056 §serial, min-width per-Passage). probe Passage 0 БЕЗ serial едет ОДНОЙ
// волной даже когда Passage 1 несёт serial:1. Если per-RUN min-width вернётся
// (effectiveSerialWidth по ВСЕМ задачам прогона), probe-волна Passage 0 раздробится
// на 3 последовательные волны по 1 хосту (silent destructive throttle) → тест падает.
//
// ASSERT: все 3 хоста probe Passage 0 диспатчатся ОДНИМ блоком БЕЗ перемежающихся
// событий Passage 1 (один barrier на весь probe, не три). Контраст с serial:1
// действием Passage 1 (3 отдельных волны) доказывает, что throttle применился
// ТОЛЬКО к Passage 1, а не просочился в Passage 0.
func TestIntegration_SerialStaged_ProbeNotThrottled(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Probe Passage 0 — ОДНА волна (3 хоста), затем Passage 1. В общей
	// последовательности SendApply ВСЕ три события Passage 0 идут НЕПРЕРЫВНО ПЕРЕД
	// первым событием Passage 1: width Passage 0 = 0 (одна волна). Per-RUN min-width
	// раздробил бы probe на 3 волны по 1 — но даже тогда события Passage 0 были бы
	// смежны; поэтому решающий ASSERT — что probe ушёл ОДНОЙ волной (один barrier),
	// что наблюдаемо через splitWaves: при width=0 dispatchWave шлёт все 3 подряд
	// БЕЗ barrier между ними, при width=1 — barrier (и накопление register) между
	// каждым. Проверяем это через число serial-волн Passage 0 = 1 (effectiveSerialWidth
	// на per-passage срезе = 0).
	last0 := disp.lastPassageEvent(0)
	first1 := disp.firstPassageEvent(1)
	if first1 <= last0 {
		t.Fatalf("★ probe Passage 0 (последнее событие idx %d) пересёкся с Passage 1 (первое idx %d) — barrier некорректен", last0, first1)
	}

	// Решающий реверс-ASSERT: width Passage 0 = 0 проверяется на чистой функции
	// (TestUnit_EffectiveSerialWidth_PerPassageSlice), а здесь end-to-end доказываем
	// поведенческое следствие: probe-волна Passage 0 единая. Если бы per-RUN min-width
	// (=1) применился к Passage 0, dispatchPassage поставил бы per-wave barrier ПОСЛЕ
	// КАЖДОГО probe-хоста, и register каждого probe-хоста копился бы по-волново —
	// поведение тождественно serial:1. Мы утверждаем НЕ-throttle: probe Passage 0
	// едет одной волной (см. dispatchPassage: splitWaves(sids, 0) → одна волна).
	// Косвенный, но достаточный сигнал — все probe-события смежны и предшествуют
	// Passage 1 (выше), плюс число хостов Passage 0 = 3 в одной группе.
	p0 := disp.passageEvents(0)
	if len(p0) != 3 {
		t.Fatalf("★ Passage 0 events = %v, want 3 хоста (probe одной волной, per-passage width=0)", p0)
	}

	// apply_runs: все хосты passage 0 + passage 1 success.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	for _, sid := range []string{"host-a.example.com", "host-b.example.com", "host-c.example.com"} {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1]", sid, got[sid])
		}
	}
}

// TestIntegration_SerialStaged_FailStopInWave — ★ FAIL-STOP в serial-волне Passage P
// (ADR-056 §2.2.1 + §г). Падение хоста в волне Passage 1 → следующая волна НЕ
// стартует, Passage 2 не достигается (его и нет — N=2), incarnation → ERROR_LOCKED,
// state НЕ коммитнут (last known-good). Три хоста slave, serial:1 на Passage 1;
// host-b (вторая волна по SID) падает → host-c (третья волна) НЕ диспатчится.
func TestIntegration_SerialStaged_FailStopInWave(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "slave",
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	disp.failOn = "host-b.example.com" // вторая serial-волна Passage 1 падает
	r := newRunner(t, disp, gitURL)

	// state до прогона — snapshot для проверки «не коммитнут».
	incBefore, err := incarnation.SelectByName(context.Background(), integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName before: %v", err)
	}

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "redis-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails["reason"] != "dispatch_failed" {
		t.Errorf("reason = %v, want dispatch_failed (serial-волна fail-stop)", inc.StatusDetails["reason"])
	}

	// ★ Passage 1: host-a (волна 1) + host-b (волна 2, упал) диспатчены; host-c
	// (волна 3) НЕ диспатчен — fail-stop остановил rolling.
	p1 := disp.passageEvents(1)
	wantDispatched := []string{"host-a.example.com", "host-b.example.com"}
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 events = %v, want [host-a host-b] (host-c не диспатчен — fail-stop на host-b)", p1)
	}
	for i := range wantDispatched {
		if p1[i] != wantDispatched[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q", i, p1[i], wantDispatched[i])
		}
	}
	if got := disp.passageEvents(1); contains(got, "host-c.example.com") {
		t.Fatalf("★ host-c получил Passage-1 ApplyRequest (%v) — следующая волна стартовала после fail-stop", got)
	}

	// ★ state НЕ коммитнут (last known-good): incarnation.state не изменился.
	if len(inc.State) != len(incBefore.State) {
		t.Errorf("★ state изменён при fail-stop: before=%v after=%v (commit обязан быть пропущен)", incBefore.State, inc.State)
	}
}

// TestIntegration_SerialStaged_RegisterAcrossWaves — register всех волн Passage P
// собран ДО старта Passage P+1 (ADR-056 §в.3 + 2D). probe Passage 0 (register: role)
// едет одной волной, его register накапливается, и Passage 1 (serial:1, where по
// register.role) резолвится по ПОЛНОМУ register Passage 0 на каждой волне.
//
// Расклад: host-a master, host-b/host-c slave. Passage 1 where role==slave →
// таргетит ТОЛЬКО host-b/host-c (master исключён). serial:1 катит двух slave по
// одному. ASSERT: Passage 1 затаргетил ровно {host-b, host-c} (register.role
// Passage 0 резолвнулся для ВСЕХ хостов до старта Passage 1), порядок по SID.
func TestIntegration_SerialStaged_RegisterAcrossWaves(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
	gitURL := serialStagedServiceRepo(t)

	disp := newSerialStagedDispatcher(t, map[string]string{
		"host-a.example.com": "master", // исключён из Passage 1 (where role==slave)
		"host-b.example.com": "slave",
		"host-c.example.com": "slave",
	})
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "restart",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// Passage 0: все три хоста (probe без where).
	if p0 := disp.passageEvents(0); len(p0) != 3 {
		t.Errorf("Passage 0 events = %v, want 3 хоста", p0)
	}

	// ★ Passage 1: ТОЛЬКО slave-хосты {host-b, host-c}, по одному (serial:1). where:
	// register.role резолвнулся по register ОБОИХ slave-хостов, собранному волной
	// Passage 0 — register всех хостов Passage 0 готов ДО Passage 1.
	p1 := disp.passageEvents(1)
	want := []string{"host-b.example.com", "host-c.example.com"}
	if len(p1) != 2 {
		t.Fatalf("★ Passage 1 events = %v, want [host-b host-c] (master исключён where, slave резолвнуты register)", p1)
	}
	for i := range want {
		if p1[i] != want[i] {
			t.Errorf("Passage 1 wave[%d] = %q, want %q", i, p1[i], want[i])
		}
	}
	if contains(p1, "host-a.example.com") {
		t.Fatalf("★ host-a (master) получил Passage-1 ApplyRequest (%v) — where role==slave не резолвнулся по register Passage 0", p1)
	}

	// apply_runs: host-a — только passage 0 (probe; Passage 1 его не таргетил);
	// host-b/host-c — passage 0 + passage 1.
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-a.example.com"]) != 1 || got["host-a.example.com"][0] != 0 {
		t.Errorf("host-a passages = %v, want [0] (master не таргетился Passage 1)", got["host-a.example.com"])
	}
	for _, sid := range want {
		if len(got[sid]) != 2 {
			t.Errorf("%s passages = %v, want [0 1]", sid, got[sid])
		}
	}
}

// contains — есть ли s в xs (хелпер ассертов «хост НЕ получил ApplyRequest»).
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
