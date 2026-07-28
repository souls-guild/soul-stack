package handlers

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// TestCoreModuleDocs_VerbShellStatesMatchSingleSource — the module catalog's
// `ErrandSafeStates` are hand-maintained doc data (keeper does not import
// soul-side core, ADR-011), so nothing but a test keeps them aligned with the
// verb-shell set the Errand runner and the console gate actually use (NIM-197).
//
// A drift here is not cosmetic: the catalog is what the UI's Run→Command picker
// reads, so a module listed as errand-safe but absent from the shared set would
// be offered to an operator and then refused by the gate — or, worse, the
// reverse.
func TestCoreModuleDocs_VerbShellStatesMatchSingleSource(t *testing.T) {
	t.Parallel()

	// The catalog's errand-safe entries that correspond to a verb-shell address.
	inCatalog := map[string]bool{}
	for _, doc := range coreModuleDocs {
		for _, state := range doc.ErrandSafeStates {
			full := doc.Name + "." + state
			if coremanifest.IsVerbShell(full) {
				inCatalog[full] = true
			}
		}
	}

	for _, full := range coremanifest.VerbShellModules() {
		if !inCatalog[full] {
			t.Errorf("%s is a verb-shell module but the catalog does not publish it as errand-safe", full)
		}
	}
	if len(inCatalog) != len(coremanifest.VerbShellModules()) {
		t.Errorf("catalog publishes %d verb-shell states, the single source has %d",
			len(inCatalog), len(coremanifest.VerbShellModules()))
	}
}
