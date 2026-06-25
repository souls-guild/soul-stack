//go:build integration

// Mid-run re-resolve roster — живое доказательство S3 (ADR-0061). Целевой инвариант:
// после успеха `core.soul.registered`-шага с `refresh_soulprint: true` (Passage 0)
// scenario-runner ПЕРЕ-резолвит roster ПЕРЕД следующим Passage, и созданные+
// онбордившиеся хосты становятся видны Passage-1-задачам через soulprint.hosts /
// on:[incarnation.name]. Рост roster эмулируется callback-ом keeper-модуля: он
// seed-ит новый connected-soul ВО ВРЕМЯ keeper-dispatch (Passage 0), как онбординг
// поднял бы новую VM. Re-resolve перед Passage 1 читает выросший SQL-roster
// (Topology=NewResolver(pool, nil) → SQL-presence по status='connected').
//
// ★ S3-инвариант: в пределах Passage 0 roster неизменен (re-resolve только на
// refresh-границе); Passage 1 видит выросший набор. assert-топология после refresh
// тоже видит рост (ADR-0061 §детерминизм).

package scenario

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// seedingKeeperModule — keeper-side core-модуль `core.soul`, который при Apply
// вызывает onApply (seed нового хоста = эмуляция онбординга созданной VM), затем
// отдаёт success-output с echo refreshed (как реальный registered.go). Симулирует
// мост provision→онбординг: к моменту барьера Passage 0 новый хост уже в souls.
type seedingKeeperModule struct {
	module.BaseModule
	onApply func()
}

func (m *seedingKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if m.onApply != nil {
		m.onApply()
	}
	out := mustStructAny(map[string]any{"refreshed": true, "created": true})
	return stream.Send(&pluginv1.ApplyEvent{Changed: true, Output: out})
}

// rosterTargetDispatcher — лёгкий Soul-симулятор для re-resolve-тестов: на каждый
// SendApply фиксирует (passage → SID) и терминалит apply_runs-строку success (как
// correlateRunResult). Без register-логики — Passage 1 этих фикстур register не
// читает (таргетинг по roster, не по probe).
type rosterTargetDispatcher struct {
	t  *testing.T
	mu sync.Mutex
	by map[int][]string
}

func newRosterTargetDispatcher(t *testing.T) *rosterTargetDispatcher {
	return &rosterTargetDispatcher{t: t, by: map[int][]string{}}
}

func (d *rosterTargetDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	passage := int(req.GetPassage())
	d.mu.Lock()
	d.by[passage] = append(d.by[passage], sid)
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, passage, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("rosterTargetDispatcher: UpdateStatus(%s, passage=%d): %v", sid, passage, err)
	}
	return nil
}

func (d *rosterTargetDispatcher) targets(passage int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := append([]string(nil), d.by[passage]...)
	return out
}

// newRunnerKeeperStaged собирает Runner с keeper-side Registry И stubPassageCap
// (staged-гейт S5 требует passage-capability). Сочетание, которого нет в готовых
// конструкторах: re-resolve-тесты несут И keeper-задачу (refresh-эмиттер), И
// staged-стратификацию (Count=2 по refresh-границе).
func newRunnerKeeperStaged(t *testing.T, disp ApplyDispatcher, keepers KeeperModuleRegistry) *Runner {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return NewRunner(Deps{
		Loader:        artifact.NewServiceLoader(t.TempDir(), nil),
		Topology:      topology.NewResolver(integrationPool, nil, nil),
		Essence:       essence.NewResolver(nil),
		Render:        render.NewPipeline(nil, engine, nil, nil),
		Outbound:      disp,
		KeeperModules: keepers,
		DB:            integrationPool,
		PassageCap:    stubPassageCap{},
		PollInterval:  20 * time.Millisecond,
		RunTimeout:    20 * time.Second,
	})
}

// refreshServiceRepo — service-repo со scenario `grow`: Passage 0 — keeper-шаг
// core.soul.registered с refresh_soulprint:true (register: roster); Passage 1 —
// soul-задача core.exec.run на on:[incarnation.name] (роль на весь выросший roster).
// S2 загоняет Passage-1-задачу строго ПОСЛЕ refresh-шага (refresh-граница).
func refreshServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: grow
description: provision-refresh-role single run (ADR-0061 §S3)
state_changes: {}
tasks:
  - name: Register and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to grown incarnation roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: "false"
    params:
      cmd: echo
      args: ["role"]
`)
}

// TestIntegration_MidRunReResolve_GrownRosterVisibleNextPassage — ★ S3 PROOF
// (ADR-0061). Стартовый roster — host-a (1 хост). Refresh-шаг (Passage 0) seed-ит
// host-c (эмуляция онбординга созданной VM). ASSERT: Passage 1 (on:[incarnation.name])
// затаргетился на ОБА хоста (host-a + host-c) — re-resolve перед Passage 1 увидел
// выросший roster. Passage 0 (keeper-шаг) хостов не таргетит (on: keeper). В пределах
// Passage 0 roster был host-a (host-c появился только перед Passage 1).
func TestIntegration_MidRunReResolve_GrownRosterVisibleNextPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})

	// keeper-модуль seed-ит host-c при Apply (= онбординг создал новую VM, она
	// online к барьеру Passage 0). re-resolve перед Passage 1 её увидит.
	var seedOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		seedOnce.Do(func() {
			seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1 (роль) затаргетился на ОБА хоста — re-resolve увидел host-c.
	p1 := disp.targets(1)
	gotSet := map[string]bool{}
	for _, sid := range p1 {
		gotSet[sid] = true
	}
	if !gotSet["host-a.example.com"] || !gotSet["host-c.example.com"] || len(p1) != 2 {
		t.Fatalf("★ Passage 1 targets = %v, want оба [host-a host-c] — re-resolve НЕ увидел выросший roster (host-c онбордился на refresh-границе)", p1)
	}

	// apply_runs: host-a + host-c обе имеют Passage-1 строку success. keeper-строка
	// (sid=keeper, passage 0) — refresh-шаг. host-c НЕ имеет Passage-0 строки
	// (он появился только перед Passage 1 — в Passage 0 его не было в roster).
	got := passagesBySID(t, applyID, applyrun.StatusSuccess)
	if len(got["host-c.example.com"]) != 1 || got["host-c.example.com"][0] != 1 {
		t.Errorf("host-c passages = %v, want [1] (появился на refresh-границе, в Passage 0 его НЕ было)", got["host-c.example.com"])
	}
	hostAHasP1 := false
	for _, p := range got["host-a.example.com"] {
		if p == 1 {
			hostAHasP1 = true
		}
	}
	if !hostAHasP1 {
		t.Errorf("host-a passages = %v, want содержащее 1 (роль применена на выросший roster)", got["host-a.example.com"])
	}
}

// TestIntegration_MidRunReResolve_NoGrowthSameRoster — КОНТРОЛЬ: refresh-шаг есть,
// но новых хостов онбординг не дал (live-снимок не изменился). re-resolve
// выполняется, но возвращает тот же набор — Passage 1 таргетит исходный roster,
// прогон успешен. Доказывает, что re-resolve на refresh-границе безопасен и при
// отсутствии изменений (live-снимок неизменного online-набора = тот же roster).
func TestIntegration_MidRunReResolve_NoGrowthSameRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

	// refresh-шаг ничего не seed-ит (онбординг не добавил хостов).
	mod := &seedingKeeperModule{onApply: func() {}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	p1 := disp.targets(1)
	if len(p1) != 2 {
		t.Fatalf("Passage 1 targets = %v, want оба исходных хоста (re-resolve без роста = тот же roster)", p1)
	}
}

// TestIntegration_MidRunReResolve_AssertSeesGrownRoster — ★ assert-топология ПОСЛЕ
// refresh видит выросший roster (ADR-0061 §детерминизм). Scenario: refresh-шаг
// (seed host-c) → assert size(soulprint.hosts) == 2. assert вычисляется Keeper-side
// на render Passage 1 ПОСЛЕ re-resolve — обязан увидеть выросший набор (2 хоста).
// Если бы re-resolve не сработал, assert упал бы (1 хост) → error_locked.
func TestIntegration_MidRunReResolve_AssertSeesGrownRoster(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})

	var seedOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		seedOnce.Do(func() {
			seedConnectedSoul(t, "host-c.example.com", []string{"redis-prod"})
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	gitURL := writeServiceRepo(t, `name: grow_assert
description: refresh then assert grown topology
state_changes: {}
tasks:
  - name: Register and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Assert grown topology
    assert:
      that:
        - "size(soulprint.hosts) == 2"
      message: "expected 2 hosts after onboarding"
`)

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// assert прошёл (2 хоста после re-resolve) → прогон успешен. Если бы re-resolve
	// не сработал, assert увидел бы 1 хост → false → error_locked.
	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)
}

// TestIntegration_MidRunReResolve_OfflineHostExcludedNextPassage — ★ КОНТРАКТ
// live-snapshot (ADR-0061 §S3): re-resolve на refresh-границе — НЕ монотонный рост,
// а СВЕЖИЙ live-снимок текущего online-набора. P0-roster хост, ушедший OFFLINE к
// refresh-границе, ИСКЛЮЧАЕТСЯ из P1-roster — таргетинг идёт на реально-online набор
// (на offline-хост роль катить не надо). Документирует ПРАВИЛЬНУЮ семантику (не
// регресс): зеркало GrownRosterVisibleNextPassage, но в обратную сторону (хост ушёл,
// а не пришёл).
//
// Seed: host-a + host-b online в P0. refresh-шаг (Passage 0) переводит host-b в
// status=disconnected (эмуляция: к refresh-границе host-b упал — потерял EventStream/
// lease). re-resolve перед Passage 1 читает live-снимок (filterAlive → status-снимок,
// lease==nil в unit-резолвере) → видит только host-a. ASSERT: Passage 1 затаргетился
// ТОЛЬКО на host-a; host-b в P1 НЕ попал.
func TestIntegration_MidRunReResolve_OfflineHostExcludedNextPassage(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"redis-prod"})

	// refresh-шаг роняет host-b offline ВО ВРЕМЯ keeper-dispatch Passage 0 (до
	// re-resolve на границе Passage 1). re-resolve читает live-снимок → host-b
	// больше не online → исключён из P1-roster.
	var dropOnce sync.Once
	mod := &seedingKeeperModule{onApply: func() {
		dropOnce.Do(func() {
			if err := soul.UpdateStatus(context.Background(), integrationPool,
				"host-b.example.com", soul.StatusDisconnected, nil); err != nil {
				t.Errorf("UpdateStatus(host-b → disconnected): %v", err)
			}
		})
	}}
	keepers := fakeKeeperRegistry{"core.soul": mod}

	disp := newRosterTargetDispatcher(t)
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "redis-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: refreshServiceRepo(t), Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "redis-prod", applyID, incarnation.StatusReady)

	// ★ Passage 1 (роль) затаргетился ТОЛЬКО на host-a — live-снимок re-resolve
	// исключил ушедший offline host-b (НЕ монотонный рост: набор может УМЕНЬШИТЬСЯ).
	p1 := disp.targets(1)
	if len(p1) != 1 || p1[0] != "host-a.example.com" {
		t.Fatalf("★ Passage 1 targets = %v, want [host-a] — live-снимок re-resolve обязан ИСКЛЮЧИТЬ ушедший offline host-b (ADR-0061 §S3: re-resolve = live-snapshot, не монотонный рост)", p1)
	}
}
