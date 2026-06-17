//go:build integration

// Integration-тесты на [Runner.ensureTerminalApplyRun] — обработка running-строк
// ПО ПРИЧИНЕ abort-а (keepRunning). Закрывают регрессию: при operator-Cancel
// running оставляем нетронутым (живой хост, дойдёт честный RunResult), при
// timeout/dead-host force-фейлим (RunResult не придёт, reaper running не
// подбирает — ADR-027 сужен до claimed → иначе apply_run висит вечно, BAG-1
// recovery). Матрица:
//   - errCancelRequested (keepRunning=true): planned/claimed/dispatched →
//     cancelled; running → НЕ трогать.
//   - timeout/ctx/прочее (keepRunning=false): planned/claimed/dispatched →
//     failed; running → failed (force-fail).
// Reuse harness-а integration_test.go (TestMain/integrationPool/resetAll/seed*).

package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// seedApplyRunRow вставляет строку apply_runs в произвольном (в т.ч. running)
// статусе для прямого теста терминализации. applyrun.Insert не валидирует статус
// на «недотерминальность», что и нужно — фиксируем running до вызова метода.
func seedApplyRunRow(t *testing.T, applyID, sid string, status applyrun.Status) {
	t.Helper()
	run := applyrun.ApplyRun{
		ApplyID:         applyID,
		SID:             sid,
		IncarnationName: "noop-prod",
		Scenario:        "create",
		Status:          status,
		StartedByAID:    startedByPtr("archon-alice"),
	}
	if err := applyrun.Insert(context.Background(), integrationPool, &run); err != nil {
		t.Fatalf("seedApplyRunRow(%s=%s): %v", sid, status, err)
	}
}

func statusByApplyID(t *testing.T, applyID string) map[string]applyrun.Status {
	t.Helper()
	rows, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	out := make(map[string]applyrun.Status, len(rows))
	for _, r := range rows {
		out[r.SID] = r.Status
	}
	return out
}

// TestIntegration_EnsureTerminal_TimeoutForceFailsRunning — регрессия (architect-
// совет): non-cancel abort (timeout/dead-host) force-фейлит running-строку, не
// оставляя её висеть вечно. Симметрично NoClaim_BarrierTimeout, но с running-
// строкой в момент терминализации. Все недотерминальные → failed.
func TestIntegration_EnsureTerminal_TimeoutForceFailsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-dispatched.example.com", applyrun.StatusDispatched)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	r := &Runner{deps: Deps{DB: integrationPool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=false (timeout): force-fail в т.ч. running.
	r.ensureTerminalApplyRun(context.Background(), spec, "run_timeout", applyrun.StatusFailed, false, slog.Default())

	got := statusByApplyID(t, applyID)
	if got["host-running.example.com"] != applyrun.StatusFailed {
		t.Errorf("running host = %q, want failed (BAG-1: timeout force-фейлит running, RunResult не придёт)", got["host-running.example.com"])
	}
	if got["host-dispatched.example.com"] != applyrun.StatusFailed {
		t.Errorf("dispatched host = %q, want failed", got["host-dispatched.example.com"])
	}
	if got["host-claimed.example.com"] != applyrun.StatusFailed {
		t.Errorf("claimed host = %q, want failed", got["host-claimed.example.com"])
	}
	// success — уже терминальна, не трогаем.
	if got["host-success.example.com"] != applyrun.StatusSuccess {
		t.Errorf("success host = %q, want success (терминал не перезаписываем)", got["host-success.example.com"])
	}
}

// TestIntegration_EnsureTerminal_CancelKeepsRunning — operator-Cancel: running на
// ЖИВОМ хосте НЕ трогаем (honest reporting, дойдёт RunResult), недотерминальные
// (planned/claimed/dispatched) → cancelled. Проверяет cancel→cancelled путь
// ИМЕННО через ensureTerminalApplyRun (а не старый Acolyte-claim-путь).
func TestIntegration_EnsureTerminal_CancelKeepsRunning(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	applyID := audit.NewULID()
	seedApplyRunRow(t, applyID, "host-running.example.com", applyrun.StatusRunning)
	seedApplyRunRow(t, applyID, "host-planned.example.com", applyrun.StatusPlanned)
	seedApplyRunRow(t, applyID, "host-claimed.example.com", applyrun.StatusClaimed)
	seedApplyRunRow(t, applyID, "host-success.example.com", applyrun.StatusSuccess)

	r := &Runner{deps: Deps{DB: integrationPool}}
	spec := RunSpec{ApplyID: applyID, IncarnationName: "noop-prod", ScenarioName: "create", StartedByAID: "archon-alice"}

	// keepRunning=true (operator-Cancel): planned/claimed → cancelled, running — keep.
	r.ensureTerminalApplyRun(context.Background(), spec, "cancelled", applyrun.StatusCancelled, true, slog.Default())

	got := statusByApplyID(t, applyID)
	if got["host-running.example.com"] != applyrun.StatusRunning {
		t.Errorf("running host = %q, want running (Cancel НЕ трогает живой apply — дойдёт честный RunResult)", got["host-running.example.com"])
	}
	if got["host-planned.example.com"] != applyrun.StatusCancelled {
		t.Errorf("planned host = %q, want cancelled (operator-Cancel)", got["host-planned.example.com"])
	}
	if got["host-claimed.example.com"] != applyrun.StatusCancelled {
		t.Errorf("claimed host = %q, want cancelled (operator-Cancel)", got["host-claimed.example.com"])
	}
	// success — уже терминальна, cancel её не перезаписывает (симметрично timeout-тесту).
	if got["host-success.example.com"] != applyrun.StatusSuccess {
		t.Errorf("success host = %q, want success (терминал не перезаписываем при cancel)", got["host-success.example.com"])
	}
}

// TestUnit_FailureTerminalStatus — sentinel-граница: errCancelRequested →
// cancelled, всё прочее (ctx.Err/RunTimeout/произвольный provider-fail) → failed.
// Тот же sentinel, по которому считается keepRunning в abort-е.
func TestUnit_FailureTerminalStatus(t *testing.T) {
	if got := failureTerminalStatus(errCancelRequested); got != applyrun.StatusCancelled {
		t.Errorf("errCancelRequested → %q, want cancelled", got)
	}
	if got := failureTerminalStatus(context.DeadlineExceeded); got != applyrun.StatusFailed {
		t.Errorf("DeadlineExceeded → %q, want failed", got)
	}
	if got := failureTerminalStatus(context.Canceled); got != applyrun.StatusFailed {
		t.Errorf("Canceled → %q, want failed", got)
	}
}
