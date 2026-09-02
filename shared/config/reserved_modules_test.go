package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// A definition may not ask for a module under a reserved name. Before NIM-377 the
// artifact declared its own namespace, so `core` could not be claimed; now address
// level 1 is an alias an operator picks, and `core.redis` in a definition is a name a
// registration could be talked into handing to a third-party binary.

func destinyDiags(t *testing.T, src string) []diag.Diagnostic {
	t.Helper()
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	return diags
}

func serviceDiags(t *testing.T, src string) []diag.Diagnostic {
	t.Helper()
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	return diags
}

func codeAt(ds []diag.Diagnostic, code, yamlPath string) *diag.Diagnostic {
	for i := range ds {
		if ds[i].Code == code && ds[i].YAMLPath == yamlPath {
			return &ds[i]
		}
	}
	return nil
}

func TestRequiredModules_ReservedNameIsRejected(t *testing.T) {
	// Every reserved name, not a sample: the list is closed and the check is a map
	// lookup, so a member that slipped past would do so silently.
	for _, name := range plugin.ReservedNames() {
		src := "name: x\nrequired_modules: [" + name + ".widget]\n"
		diags := destinyDiags(t, src)
		if !diag.HasErrors(diags) {
			t.Errorf("required_modules: [%s.widget] passed validation: %v", name, diagCodesP(diags))
			continue
		}
		want := "reserved_module_namespace"
		if name == "core" {
			want = "core_module_in_modules_list"
		}
		if codeAt(diags, want, "$.required_modules[0]") == nil {
			t.Errorf("required_modules: [%s.widget] → %v, want %s", name, diagCodesP(diags), want)
		}
	}
}

// The reserved check must beat the format check on a name that is regex-valid: telling
// the author "does not match <alias>.<module>" about `core.haproxy` sends them
// hunting for a typo in a string they spelled exactly as they meant it.
func TestRequiredModules_ReservedBeatsFormatDiagnostic(t *testing.T) {
	diags := destinyDiags(t, "name: x\nrequired_modules: [core.haproxy]\n")
	if hasCode(diags, "required_module_invalid_format") {
		t.Errorf("core.haproxy reported as a format error: %v", diagCodesP(diags))
	}
}

// An ordinary third-party address is untouched — the check must not have widened into
// "two-level names are suspicious".
func TestRequiredModules_OrdinaryAddressIsAccepted(t *testing.T) {
	diags := destinyDiags(t, "name: x\nrequired_modules: [acme.haproxy, community.redis]\n")
	if diag.HasErrors(diags) {
		t.Errorf("a plain plugin address was rejected: %v", diags)
	}
}

func TestServiceModules_ReservedNameIsRejected(t *testing.T) {
	const head = "name: x\nstate_schema_version: 1\nstate_schema: {}\n"
	for _, name := range plugin.ReservedNames() {
		diags := serviceDiags(t, head+"modules:\n  - { name: "+name+".widget, ref: v1 }\n")
		want := "reserved_module_namespace"
		if name == "core" {
			// Unchanged code and sentence: `core` is the one reserved name an author
			// reaches for by accident, and it is in the published error catalog.
			want = "core_module_in_modules_list"
		}
		if codeAt(diags, want, "$.modules[0].name") == nil {
			t.Errorf("modules[0].name = %s.widget → %v, want %s", name, diagCodesP(diags), want)
		}
		if hasCode(diags, "name_invalid_format") {
			t.Errorf("%s.widget also raised name_invalid_format: %v", name, diagCodesP(diags))
		}
	}
}

// A destiny dependency is a single-level name, so the two-level reserved test must not
// fire on one that happens to spell a reserved word.
func TestServiceDestiny_ReservedWordAsDestinyNameIsNotAModuleAddress(t *testing.T) {
	const head = "name: x\nstate_schema_version: 1\nstate_schema: {}\n"
	diags := serviceDiags(t, head+"destiny:\n  - { name: core, ref: v1 }\n")
	if hasCode(diags, "reserved_module_namespace") || hasCode(diags, "core_module_in_modules_list") {
		t.Errorf("a destiny named `core` was treated as a module address: %v", diagCodesP(diags))
	}
}

// The hint names the closed list, so an author who picked a reserved word can see what
// else is off-limits without opening the source.
func TestReservedModuleDiag_HintCarriesTheList(t *testing.T) {
	diags := destinyDiags(t, "name: x\nrequired_modules: [sigil.widget]\n")
	d := codeAt(diags, "reserved_module_namespace", "$.required_modules[0]")
	if d == nil {
		t.Fatalf("no reserved_module_namespace: %v", diagCodesP(diags))
	}
	for _, want := range []string{"core", "keeper", "soul"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("hint does not mention reserved name %q: %s", want, d.Hint)
		}
	}
}

// Set equality against the list NIM-377 settled, held here because
// `shared/plugin` did not land a test of its own (the ticket allowed one new file
// there) and because every other guard in this package iterates
// [plugin.ReservedNames] — they would all stay green if a name quietly left the
// list, which is the one change that reopens the shadowing.
//
// Failing here is not automatically a bug: the list is closed and adding to it is
// propose-and-wait plus a docs PR (naming-rules.md "Reserved namespace names").
// If the list legitimately moved, move this with it and say so out loud.
func TestReservedNames_MatchTheSettledList(t *testing.T) {
	want := map[string]bool{
		// Tier 1 — engine namespaces, the ones that shadow a real address.
		"core": true, "keeper": true, "soul": true,
		// Tier 2 — the Soul Stack dictionary and the usual collisions.
		"destiny": true, "scenario": true, "service": true, "incarnation": true,
		"soulprint": true, "coven": true, "archon": true, "sigil": true,
		// NIM-706: `herald` and `provider` joined tier 2. Both are dictionary entities,
		// and the platform writes a Vault path family under each name
		// (keeper/internal/secretwrite), which is what made them worth taking before
		// someone registers an alias by them. docs/naming-rules.md moved with this.
		"herald": true, "provider": true,
		"soulstack": true, "soul-stack": true, "local": true, "default": true,
		"internal": true, "test": true, "example": true,
	}
	got := map[string]bool{}
	for _, n := range plugin.ReservedNames() {
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("reserved list lost %q — an operator can now register that alias", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("reserved list gained %q; adding to a closed list is propose-and-wait plus a docs PR", n)
		}
	}
}

// Both surfaces in this package feed [plugin.IsReserved] a namespace taken
// straight out of a definition, with no normalising of their own and no grammar
// check first. A validator that refused `core` while waving `Core.widget`
// through would hand the shadowing back through the door it just closed.
func TestReservedModuleAddr_FoldsCaseAndSpace(t *testing.T) {
	for _, addr := range []string{"Core.widget", "CORE.widget", " core .widget", "KeEpEr.widget"} {
		if !reservedModuleAddr(addr) {
			t.Errorf("required_modules: [%s] slipped past the reserved check", addr)
		}
	}
	// The fold must not swallow an honest alias that merely starts the same way.
	for _, addr := range []string{"coreutils.widget", "keeperx.widget", "my-core.widget"} {
		if reservedModuleAddr(addr) {
			t.Errorf("%s was refused; only the exact reserved names are taken", addr)
		}
	}
}
