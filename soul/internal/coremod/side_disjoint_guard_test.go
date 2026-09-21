package coremod

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// TestNoSoulSideModuleIsInTheKeeperCatalog — the other half of the disjointness
// the whole derivation rests on (NIM-749).
//
// Reading the side off the module address is only sound while no core name lives
// on both sides. If one did, the catalog would route every task addressing it to
// the Keeper — including the twenty-one modules that only exist on a host — and
// the step would be dropped on an executor that cannot run it. The keeper-side
// half is guarded in `keeper/internal/coremod`; this is the side that would go
// wrong silently, because a Soul-side module wrongly listed keeper-side fails at
// dispatch rather than at build.
//
// Reads the registry this binary actually serves, not a literal list: a module
// added to `soul` without a thought about its side is caught here on the first
// run of the suite.
func TestNoSoulSideModuleIsInTheKeeperCatalog(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("the soul-side registry is empty — the guard would pass vacuously")
	}
	for _, name := range names {
		if coremanifest.IsKeeperSide(name) {
			t.Errorf("%q is served by the soul binary AND listed keeper-side — the two sets must stay disjoint, "+
				"or the module address cannot decide where a task runs", name)
		}
	}
}
