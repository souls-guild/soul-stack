//go:build integration

// Integration guard-тесты remap onchanges/onfail-индексов (ADR-056 amend, R1).
// Reuse harness-а integration_test.go / staged_integration_test.go (TestMain /
// seed* / newRunner / waitRunDone / writeServiceRepo). Доказывают:
//   - R1 ★ N=1+where реверс-guard: задача-источник, отфильтрованная where: на одном
//     хосте, не «промахивает» onchanges-индекс потребителя на ЭТОМ хосте (sentinel),
//     а на хосте, где источник присутствует — индекс ремапится на его ЛОКАЛЬНУЮ
//     позицию. Это ЛАТЕНТНЫЙ баг ВНЕ staged (N=1, where по стабильному факту).
//
// Cross-passage requisite-gating (R3, бывший R2-reject) — в crosspassage_integration_test.go.

package scenario

import (
	"context"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// onchangesCaptureDispatcher симулирует Soul и захватывает per-host onchanges_idx
// КАЖDOй задачи ApplyRequest (sid → task-name → onchanges_idx). Терминалит строку
// success (как mockDispatcher). Это позволяет проверить, что remap onchanges-индекса
// в ToProtoTasks дал ЛОКАЛЬНУЮ позицию источника в срезе ЭТОГО хоста (а не глобальный
// Index, который Soul ключевал бы registerByIdx-промахом).
type onchangesCaptureDispatcher struct {
	t  *testing.T
	mu sync.Mutex
	// byHost[sid][taskName] = onchanges_idx этой задачи в ApplyRequest хоста.
	byHost map[string]map[string][]int32
}

func newOnchangesCaptureDispatcher(t *testing.T) *onchangesCaptureDispatcher {
	return &onchangesCaptureDispatcher{t: t, byHost: map[string]map[string][]int32{}}
}

func (d *onchangesCaptureDispatcher) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	d.mu.Lock()
	m := map[string][]int32{}
	for _, task := range req.GetTasks() {
		m[task.GetName()] = task.GetOnchangesIdx()
	}
	d.byHost[sid] = m
	d.mu.Unlock()
	if err := applyrun.UpdateStatus(ctx, integrationPool, req.GetApplyId(), sid, 0, applyrun.StatusSuccess, nil); err != nil {
		d.t.Errorf("onchangesCaptureDispatcher: UpdateStatus: %v", err)
	}
	return nil
}

func (d *onchangesCaptureDispatcher) onchanges(sid, taskName string) ([]int32, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.byHost[sid]
	if !ok {
		return nil, false
	}
	idx, ok := m[taskName]
	return idx, ok
}

// TestIntegration_RemapOnChanges_N1Where_NoMisfire — ★ R1 N=1+where РЕВЕРС-GUARD
// (ADR-056 amend). ЛАТЕНТНЫЙ баг ВНЕ staged: план [config-change (where: только
// host-a, по стабильному факту soulprint.self.sid — N=1, БЕЗ register-зависимости),
// restart onchanges:[config-change] (на оба хоста)]. На host-a срез =
// [config-change(local 0), restart(local 1)] → onchanges_idx restart = [0] (локальная
// позиция источника). На host-b config-change отфильтрован → срез = [restart(local 0)]
// → onchanges_idx restart = [-1] (sentinel: источник отсутствует, Soul трактует как
// changed=false → restart НЕ мисфайрит). Реверс (БЕЗ remap): на host-b onchanges_idx
// = глобальный Index источника (0) → Soul registerByIdx[0] = САМ restart → мисфайр.
func TestIntegration_RemapOnChanges_N1Where_NoMisfire(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	seedConnectedSoul(t, "host-b.example.com", []string{"noop-prod"})

	// where: по стабильному факту (soulprint.self.sid) — НЕ register → план N=1
	// (один Passage), но config-change таргетится per-host ТОЛЬКО на host-a.
	const scn = `name: create
description: n=1 per-host where requisite remap fixture
state_changes: {}
tasks:
  - name: config-change
    module: core.exec.run
    register: cfg
    where: "soulprint.self.sid == 'host-a.example.com'"
    params: { cmd: write-config }
    changed_when: "false"
  - name: restart
    module: core.exec.run
    onchanges: [cfg]
    params: { cmd: restart }
    changed_when: "false"
`
	gitURL := writeServiceRepo(t, scn)

	disp := newOnchangesCaptureDispatcher(t)
	r := newRunner(t, disp, gitURL)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// host-a: config-change присутствует (local 0), restart (local 1). onchanges_idx
	// restart = [0] — ЛОКАЛЬНАЯ позиция источника в срезе host-a.
	if idxA, ok := disp.onchanges("host-a.example.com", "restart"); !ok {
		t.Fatalf("host-a: restart не пришёл в ApplyRequest")
	} else if len(idxA) != 1 || idxA[0] != 0 {
		t.Fatalf("host-a: restart onchanges_idx = %v, want [0] (источник config-change на локальной позиции 0)", idxA)
	}

	// ★ host-b: config-change ОТФИЛЬТРОВАН where: → в срезе ТОЛЬКО restart (local 0).
	// onchanges_idx restart = [-1] (sentinel отсутствующего источника). Реверс без
	// remap дал бы [0] → Soul registerByIdx[0]=сам restart → ложный gating (мисфайр).
	idxB, ok := disp.onchanges("host-b.example.com", "restart")
	if !ok {
		t.Fatalf("host-b: restart не пришёл в ApplyRequest (должен — onchanges не режет таргет)")
	}
	if len(idxB) != 1 || idxB[0] != -1 {
		t.Fatalf("★ host-b: restart onchanges_idx = %v, want [-1] (источник config-change отфильтрован where → sentinel; реверс без remap дал бы [0] = глобальный Index → registerByIdx-промах → restart мисфайрит)", idxB)
	}

	// host-b НЕ должен нести задачу config-change вовсе (where отфильтровал).
	if _, present := disp.onchanges("host-b.example.com", "config-change"); present {
		t.Errorf("host-b: config-change не должен был попасть в срез (where: только host-a)")
	}
}
