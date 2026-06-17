//go:build e2e

// L3a E2E: keeper-side module dispatch (ADR-017, docs/keeper/modules.md) —
// фундамент epic-а membership (S1). Доказывает, что задача с `on: keeper`
// исполняется ЛОКАЛЬНО на keeper-инстансе через keeper-side core-Registry и
// реально мутирует реестр `souls` (coven-привязка модулем core.soul.registered),
// а не уходит Soul-у.
//
// Почему ловит регрессии S1:
//   - coreReg не прокинут в scenario-runner (B1) → keeper-задача не исполнится →
//     ErrKeeperModulesNotConfigured → error_locked;
//   - render реджектит on: keeper (B2 не сделан) → render_failed → error_locked;
//   - keeper-задача исполнена, но coven не записан (B3 агрегация сломана) →
//     ассерт souls.coven падает;
//   - keeper apply_runs-строка не success → WaitApplySuccess timeout/fatal.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EKeeperSideDispatch_CovenRegistered(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-keeper-register",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "keeper-register", "examples/service/service-keeper-register")

	// Live EventStream-стрим: соседний Soul-side echo-шаг прогона диспатчится
	// сюда (default-success), keeper-шаг исполняется локально на keeper-е.
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)

	soulSID := stack.SoulSID(0)
	const incName = "test-keeper-register"
	const covenLabel = "keeper-tagged"

	// Roster прогона резолвится по корневой Coven-метке incarnation.name (ADR-008):
	// без членства scenario видит no_hosts → error_locked. Сама проверяемая метка
	// (covenLabel) приписывается keeper-side шагом.
	stack.AddSoulToCoven(t, 0, incName)

	// CreateIncarnation авто-запускает scenario `create`: keeper-side
	// core.soul.registered (on: keeper) добавляет covenLabel в souls.coven этого
	// SID + Soul-side echo на хосте.
	_, applyID := stack.CreateIncarnationWithApply(t, incName, "keeper-register@main", map[string]any{
		"soul_sid":    soulSID,
		"coven_label": covenLabel,
	})

	// Все строки прогона (keeper-target sid="keeper" + host-строка) → success.
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")

	// Главный ассерт: keeper-side шаг реально записал coven в реестр souls.
	assertSoulHasCoven(t, stack, soulSID, covenLabel)

	// keeper-target прогона существует отдельной строкой apply_runs (доказывает,
	// что keeper-side исполнение проходит через ту же apply_runs-модель).
	assertKeeperApplyRun(t, stack, applyID)
}

// assertSoulHasCoven проверяет, что souls.coven указанного SID содержит метку.
func assertSoulHasCoven(t *testing.T, stack *harness.Stack, sid, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var covens []string
	if err := stack.DB().QueryRow(ctx,
		"SELECT coven FROM souls WHERE sid = $1", sid).Scan(&covens); err != nil {
		t.Fatalf("assertSoulHasCoven %s: query: %v", sid, err)
	}
	for _, c := range covens {
		if c == label {
			return
		}
	}
	t.Fatalf("assertSoulHasCoven %s: coven=%v не содержит %q — keeper-side core.soul.registered не записал метку",
		sid, covens, label)
}

// assertKeeperApplyRun проверяет наличие success-строки apply_runs для
// keeper-target прогона (sid="keeper" = render.KeeperTargetSID).
func assertKeeperApplyRun(t *testing.T, stack *harness.Stack, applyID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	if err := stack.DB().QueryRow(ctx,
		"SELECT status FROM apply_runs WHERE apply_id = $1 AND sid = 'keeper'", applyID).Scan(&status); err != nil {
		t.Fatalf("assertKeeperApplyRun %s: нет keeper-target строки apply_runs (sid='keeper'): %v", applyID, err)
	}
	if status != "success" {
		t.Fatalf("assertKeeperApplyRun %s: keeper-target status=%q, want success", applyID, status)
	}
}
