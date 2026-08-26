package stateop

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// TestManifestAndDispatchAgree — soul-lint judges a `core.state.<verb>` task against
// the manifest in shared/coremanifest, and the module executes it against [States].
// A state or a param declared in only one of the two is the drift that costs most: a
// task green-lit offline and refused at runtime, or the reverse — accepted by the
// module while the linter calls the address unknown.
//
// The two tables are written by hand in different repositories' worth of code, so
// nothing but this test holds them together.
func TestManifestAndDispatchAgree(t *testing.T) {
	mod, ok := coremanifest.Default().Lookup(ModuleName)
	if !ok {
		t.Fatalf("%s is not in the core manifest", ModuleName)
	}

	if got, want := SortedKeys(mod.States), SortedKeys(States); !equalStrings(got, want) {
		t.Fatalf("states: manifest has %v, the module dispatches %v", got, want)
	}

	for _, name := range SortedKeys(States) {
		spec := States[name]
		// What the module accepts, in the order [CheckParams] builds it.
		accepted := map[string]bool{ParamField: true}
		if spec.TakesValue {
			accepted[ParamValue] = true
		}
		required := map[string]bool{ParamField: true, ParamValue: spec.TakesValue}
		for _, p := range spec.Required {
			accepted[p], required[p] = true, true
		}
		for _, p := range spec.Optional {
			accepted[p] = true
		}

		declared := mod.States[name].Input
		if got, want := SortedKeys(declared), SortedKeys(accepted); !equalStrings(got, want) {
			t.Errorf("%s.%s: manifest declares params %v, the module accepts %v", ModuleName, name, got, want)
			continue
		}
		for _, p := range SortedKeys(declared) {
			if declared[p].Required != required[p] {
				t.Errorf("%s.%s: param %q required=%v in the manifest, %v in the module", ModuleName, name, p, declared[p].Required, required[p])
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
