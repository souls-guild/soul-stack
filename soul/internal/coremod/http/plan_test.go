package http_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	corehttp "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
)

// TestHTTP_NotPlanReadSafe — core.http is a verb module (probe), changed is
// structurally always false. The module.PlanReadSafe marker is NOT
// implemented → the host applies default-deny (FAILED `plan.unsupported`) on
// dry_run.
func TestHTTP_NotPlanReadSafe(t *testing.T) {
	m := corehttp.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.http implements PlanReadSafe (it should not — a verb module with no desired state)")
	}
}
