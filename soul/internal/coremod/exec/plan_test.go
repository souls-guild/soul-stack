package exec_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
)

// TestExec_NotPlanReadSafe — core.exec — verb-модуль, БЕЗ desired state.
// Маркер module.PlanReadSafe НЕ реализован → host применит default-deny
// (FAILED `plan.unsupported`) на dry_run. Это конструктивный отказ, а не
// ложное «нет дрифта».
func TestExec_NotPlanReadSafe(t *testing.T) {
	m := exec.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.exec реализует PlanReadSafe (не должен — verb-модуль без desired state)")
	}
}
