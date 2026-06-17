package coremod_test

import (
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/pkg"
)

func TestDefault_ContainsAllCoreMVP(t *testing.T) {
	r := coremod.Default()
	want := []string{
		// Core.a.1
		"core.pkg", "core.file", "core.service", "core.user", "core.group",
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
		// ADR-025 — read-probe Augur
		"core.augur",
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
	r := coremod.Default()
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
