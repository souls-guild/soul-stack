package cmd_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
)

// TestCmd_NotPlanReadSafe — core.cmd — verb-модуль, БЕЗ desired state.
// Маркер module.PlanReadSafe НЕ реализован → host применит default-deny
// (FAILED `plan.unsupported`) на dry_run.
func TestCmd_NotPlanReadSafe(t *testing.T) {
	m := cmd.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.cmd реализует PlanReadSafe (не должен — verb-модуль без desired state)")
	}
}
