package coremod_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/pkg"
)

func TestDefault_ContainsAllCoreMVP(t *testing.T) {
	r := coremod.Default(installmod.Deps{})
	want := []string{
		// Core.a.1
		"core.pkg", "core.file", "core.directory", "core.service", "core.user", "core.group",
		// Core.a.2
		"core.exec", "core.cmd", "core.cron", "core.mount",
		// Core.a.3
		"core.git", "core.archive", "core.sysctl",
		// Core.a.4
		"core.url",
		// Core.a.5
		"core.line",
		// Core.a.6
		"core.repo", "core.firewall",
		// Core.a.7
		"core.http",
		// ADR-015 — no-op/barrier anchor
		"core.noop",
		// ADR-025 — read-probe Augur
		"core.augur",
		// ADR-065 — SoulModule plugin delivery
		"core.module",
	}
	for _, name := range want {
		if _, ok := r.Lookup(name); !ok {
			t.Fatalf("Lookup(%q): not registered", name)
		}
	}
	names := r.Names()
	sort.Strings(names)
	got := names
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Names size=%d want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Names[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestLookup_UnknownModule(t *testing.T) {
	r := coremod.Default(installmod.Deps{})
	if _, ok := r.Lookup("core.frobnicate"); ok {
		t.Fatal("Lookup unknown: ok=true")
	}
}

func TestNewRegistry_CopiesInput(t *testing.T) {
	src := map[string]module.SoulModule{"core.pkg": pkg.New()}
	r := coremod.NewRegistry(src)
	delete(src, "core.pkg")
	if _, ok := r.Lookup("core.pkg"); !ok {
		t.Fatal("Registry shares storage with caller's map")
	}
}

// TestNames_MatchesDefaultRegistry — the Hello announcement is built from
// [coremod.Names] (ADR-0076(i)) while dispatch resolves through the wired-up
// Default registry. If the two ever diverge, keeper gates on a set the binary
// does not actually serve — either rejecting runs it could do, or (worse)
// admitting one it cannot.
func TestNames_MatchesDefaultRegistry(t *testing.T) {
	announced := coremod.Names()
	registered := coremod.Default(installmod.Deps{
		// Non-zero deps: the announced set must not depend on host wiring.
		ModulesRoot: "/var/lib/soul-stack/modules",
	}).Names()
	sort.Strings(announced)
	sort.Strings(registered)
	if len(announced) == 0 {
		t.Fatal("Names() is empty - a soul announcing no modules is rejected for every run")
	}
	if !reflect.DeepEqual(announced, registered) {
		t.Fatalf("Names() = %v, registry = %v", announced, registered)
	}
}

// TestSoulCapabilities_CoversEveryRegisteredModule — end-to-end of the
// announcement: every module the binary can dispatch is named in what it tells
// keeper.
func TestSoulCapabilities_CoversEveryRegisteredModule(t *testing.T) {
	announced := map[string]bool{}
	for _, c := range config.SoulCapabilities(coremod.Names()) {
		announced[c] = true
	}
	for _, name := range coremod.Default(installmod.Deps{}).Names() {
		if !announced[config.ModuleCapability(name)] {
			t.Errorf("module %q is registered but not announced", name)
		}
	}
}
