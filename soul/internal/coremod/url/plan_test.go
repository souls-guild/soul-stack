package url_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/url"
)

// TestURL_NotPlanReadSafe — core.url in MVP does NOT declare PlanReadSafe:
// pure-read drift ("does this need downloading?") for the no-checksum branch
// requires a HEAD request that Apply doesn't make before mutating (see the
// Plan doc). The host applies default-deny (FAILED `plan.unsupported`) on
// dry_run — a HEAD-probe with opt-out flags symmetric to Apply is a separate
// future slice.
func TestURL_NotPlanReadSafe(t *testing.T) {
	m := url.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.url implements PlanReadSafe (should not in MVP - see doc Plan)")
	}
}
