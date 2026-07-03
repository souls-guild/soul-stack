//go:build integration

// ★ Parity-guard live-бага provisioned_vm_ids (ADR-056 amendment 2026-07-02).
// Класс, который L0-harness МАСКИРОВАЛ (trial/harness.go broadcast-ит register
// всем хостам): register keeper-side задачи копится под синтетическим SID
// "keeper" (accumulateKeeperRegister), а stateChangesVars до фикса читал только
// per-host bucket хоста → `${ register.provision.* }` в state_changes.sets →
// "no such key" → error_locked ПОСЛЕ полностью успешного деплоя. Тест идёт
// live-путём: run()+PG, реальные accumulateKeeperRegister/loadRegisterByHost/
// RenderStateOps — никакого mock-register-broadcast-а.

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// keeperRegisterStateChangesRepo — зеркало live create redis-prov: keeper-задача
// (on: keeper, register: provision) + host-задача, state_changes.sets читает
// register keeper-задачи.
func keeperRegisterStateChangesRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: keeper-register in state_changes.sets (live provisioned_vm_ids class)
state_changes:
  sets:
    provisioned_ip: "${ register.provision.ip }"
tasks:
  - name: provision vm
    module: core.bootstrap.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: role on host
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["role"]
`)
}

// TestIntegration_KeeperRegisterInSets_CommitsToState — прогон завершается Ready
// (НЕ error_locked), значение register keeper-задачи коммитится в incarnation.state.
// Per-host register хостов при этом отдельно проверяет
// TestIntegration_RegisterInSets_CommitsToState (host-probe класс).
func TestIntegration_KeeperRegisterInSets_CommitsToState(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
	gitURL := keeperRegisterStateChangesRepo(t)

	bootstrap := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

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

	// До фикса: деплой success, но state_changes-render падает "no such key:
	// provision" → error_locked (waitRunDone зафейлит статус-ассерт).
	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	if inc.State["provisioned_ip"] != "10.0.0.7" {
		t.Errorf("incarnation.state.provisioned_ip = %v, want \"10.0.0.7\" (из register.provision.ip keeper-задачи)", inc.State["provisioned_ip"])
	}
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (host-задача)", disp.calls)
	}
}
