//go:build e2e

// L3a E2E: 2-passage staged-render proof (ADR-056, S3).
//
// Минимальный staged-сценарий probe→where на контракт-тире через live gRPC-mTLS-
// стримы двух soul-stub-ов:
//   - Passage 0 — probe роли (register: role): host-0 эмитит role='master',
//     host-1 — role='slave' (per-host register через soul-stub.SetTaskRegister →
//     TaskEvent.RegisterData с echo passage);
//   - Passage 1 — действие `where: register.role.stdout == 'master'`.
//
// ASSERT: Passage-1 ApplyRequest пришёл ТОЛЬКО на master-хост (host-0) — where
// резолвнулся per-host register-ом Passage 0 end-to-end (render→dispatch→barrier→
// register→render). Это доказывает staged-render на контракт-тире: drift «register
// в where всегда пуст» (ADR-056 §Контекст) закрыт.
//
// Полный live redis-cluster (cloud/vault-scope) — НЕ здесь (S4/S5). soul-stub
// симулирует Soul (per-task register + RunResult, echo passage), реального apply
// нет (L3a-контракт).
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

func TestE2EStagedFailover_2Passage(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "tests/e2e/staged-failover", // НЕ examples/** (WIP-зона)
		Souls:       2,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "staged-failover", "tests/e2e/staged-failover")

	// Live стримы обоих soul-stub-ов: ApplyRequest каждого Passage маршрутизируется
	// в локальный Outbound нужного SID-а.
	master := stack.ConnectSoulStub(t, 0)
	slave := stack.ConnectSoulStub(t, 1)
	master.SetApplyDefaultSuccess(true)
	slave.SetApplyDefaultSuccess(true)

	// Per-host probe-register: probe-задача "Probe role" эмитит role.stdout =
	// 'master' на host-0, 'slave' на host-1 (TaskEvent.RegisterData, echo passage 0).
	master.SetTaskRegister("Probe role", map[string]any{"stdout": "master", "changed": false, "failed": false})
	slave.SetTaskRegister("Probe role", map[string]any{"stdout": "slave", "changed": false, "failed": false})

	// Coven-членство: оба хоста в incarnation-ковене (roster по incarnation.name).
	stack.AddSoulToCoven(t, 0, "test-failover")
	stack.AddSoulToCoven(t, 1, "test-failover")

	inc, createApply := stack.CreateIncarnationWithApply(t, "test-failover", "staged-failover@main", nil)
	// Auto-create-прогон (POST /v1/incarnations) должен ЗАВЕРШИТЬСЯ ДО запуска
	// failover — иначе failover отклонится «уже applying». Ждём терминала именно
	// create-apply_id, потом — ready (commit state).
	stack.WaitApplySuccess(t, createApply, 60)
	stack.WaitIncarnationReady(t, inc, 30)

	applyID := stack.RunScenario(t, inc, "failover", nil)

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")

	// ★ Passage 0 (probe): ApplyRequest пришёл ОБОИМ хостам (probe без where).
	masterP0 := applyRequestsForPassage(master, 0)
	slaveP0 := applyRequestsForPassage(slave, 0)
	if masterP0 == 0 || slaveP0 == 0 {
		t.Fatalf("Passage 0: master ApplyRequest=%d, slave ApplyRequest=%d — оба хоста должны получить probe", masterP0, slaveP0)
	}

	// ★ Passage 1 (действие): ApplyRequest пришёл ТОЛЬКО master-хосту. where:
	// register.role.stdout == 'master' резолвнулся per-host register-ом Passage 0.
	masterP1 := applyRequestsForPassage(master, 1)
	slaveP1 := applyRequestsForPassage(slave, 1)
	if masterP1 == 0 {
		t.Fatalf("★ Passage 1: master НЕ получил ApplyRequest — staged where не затаргетил master")
	}
	if slaveP1 != 0 {
		t.Fatalf("★ Passage 1: slave получил %d ApplyRequest(ов) — where: register.role=='master' НЕ должен таргетить slave (drift не закрыт)", slaveP1)
	}
}

// applyRequestsForPassage считает ApplyRequest-фреймы, пришедшие stub-у с заданным
// passage (ADR-056 echo ApplyRequest.passage). Источник — stub.Messages()
// (записанные FromKeeper-фреймы за время жизни стрима).
func applyRequestsForPassage(stub *soulstub.Stub, passage int32) int {
	n := 0
	for _, m := range stub.Messages() {
		if req := m.Frame.GetApplyRequest(); req != nil && req.GetPassage() == passage {
			n++
		}
	}
	return n
}
