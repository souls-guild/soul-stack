package exec_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
)

// TestExec_NotPlanReadSafe — core.exec is a verb module, with NO desired
// state. It doesn't implement the module.PlanReadSafe marker → the host
// applies default-deny (FAILED `plan.unsupported`) on dry_run. This is a
// deliberate rejection, not a false "no drift".
func TestExec_NotPlanReadSafe(t *testing.T) {
	m := exec.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.exec implements PlanReadSafe (it should not - verb module with no desired state)")
	}
}
