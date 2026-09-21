// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the MODULE domain (handler-native T5d).
// module no longer depends on the legacy generator — golden checks json native values against a pinned
// reference string. Covers the form-prep reply route: sids ([]string, required) + truncated;
// nil sids → wire `null`. Mutating the native-struct shape (drop a field / change a json tag /
// change a type) reddens the case.
//
// ModuleCatalogReply/Item are ALREADY native (N6, handler-local structs), not duplicated here.
package api

import (
	"encoding/json"
	"testing"
)

func goldenModuleWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_ModuleReply(t *testing.T) {
	// --- full: sids non-empty + truncated ---
	sids := []string{"web1.example.com", "web2.example.com"}
	goldenModuleWire(t, "FormPrepReply/full",
		ModuleFormPrepReply{Sids: sids, Truncated: true},
		`{"sids":["web1.example.com","web2.example.com"],"truncated":true}`)
	// --- nil: empty set (handler returns a sorted slice; nil → wire `null`) ---
	goldenModuleWire(t, "FormPrepReply/nil_sids",
		ModuleFormPrepReply{Sids: nil, Truncated: false},
		`{"sids":null,"truncated":false}`)
}
