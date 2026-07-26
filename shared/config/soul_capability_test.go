package config

import (
	"reflect"
	"sort"
	"testing"
)

// The announcement is the Soul-side half of the engine-compat contract
// (ADR-0076(i)): keeper derives what a run needs from the rendered plan and
// checks it against exactly these strings. A drift here is a silent fail-closed
// on every run, so the shape is pinned.

func TestSoulCapabilities_FeaturesAndModules(t *testing.T) {
	got := SoulCapabilities([]string{"core.pkg", "core.file"})
	want := []string{
		"console", "dry_run", "flow_control",
		"module:core.file", "module:core.pkg",
		"passage", "retry",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SoulCapabilities = %v, want %v", got, want)
	}
}

// Sorted and deduplicated: Registry.Names() iterates a Go map, so an unsorted
// announcement would differ between reconnects of the SAME binary and make the
// persisted set look like a version change.
func TestSoulCapabilities_StableAcrossRegistryOrder(t *testing.T) {
	a := SoulCapabilities([]string{"core.pkg", "core.file", "core.exec"})
	b := SoulCapabilities([]string{"core.exec", "core.pkg", "core.file", "core.pkg"})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("announcement depends on input order/duplicates: %v vs %v", a, b)
	}
	if !sort.StringsAreSorted(a) {
		t.Fatalf("announcement is not sorted: %v", a)
	}
}

// A binary with no modules still announces its protocol and DSL features — the
// two groups are independent.
func TestSoulCapabilities_NoModules(t *testing.T) {
	got := SoulCapabilities(nil)
	want := []string{"console", "dry_run", "flow_control", "passage", "retry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SoulCapabilities(nil) = %v, want %v", got, want)
	}
}

func TestModuleCapability_NoStateSuffix(t *testing.T) {
	if got := ModuleCapability("core.pkg"); got != "module:core.pkg" {
		t.Fatalf("ModuleCapability = %q, want module:core.pkg", got)
	}
}
