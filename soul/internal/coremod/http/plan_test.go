package http_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	corehttp "github.com/souls-guild/soul-stack/soul/internal/coremod/http"
)

// TestHTTP_NotPlanReadSafe — core.http — verb-модуль (probe), changed
// конструктивно всегда false. Маркер module.PlanReadSafe НЕ реализован → host
// применит default-deny (FAILED `plan.unsupported`) на dry_run.
func TestHTTP_NotPlanReadSafe(t *testing.T) {
	m := corehttp.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.http реализует PlanReadSafe (не должен — verb-модуль без desired state)")
	}
}
