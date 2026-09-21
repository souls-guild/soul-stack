package main

import (
	"reflect"
	"testing"
)

// TestSessionDeps_FieldRoster pins the dependency roster of [sessionDeps].
//
// Keyed fields make a swapped pair of same-typed dependencies unexpressible,
// but they have the mirror blind spot: a newly added field is simply zero at
// every call site and nothing forces the author back to the wire-up. That is
// the NIM-207 shape — the loop grew two dependencies and its test harness kept
// compiling without them.
//
// This test deliberately lives in the default build: the harness that drifted
// (failback_integration_test.go) sits behind the `integration` tag, so a plain
// `make check` never compiled it and had nothing to notice.
func TestSessionDeps_FieldRoster(t *testing.T) {
	want := []string{
		"runner",
		"errandRunner",
		"scheduler",
		"soulprintPush",
		"utilizationPulse",
		"sigils",
		"anchors",
		"consoleMetrics",
		"streamMetrics",
		"notifier",
		"logger",
	}

	typ := reflect.TypeOf(sessionDeps{})
	got := make([]string, typ.NumField())
	for i := range got {
		got[i] = typ.Field(i).Name
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessionDeps roster changed:\n got: %v\nwant: %v\n\n"+
			"A dependency was added, removed, renamed or reordered. Update the roster "+
			"above AND revisit every sessionDeps literal: runDaemon in main.go, and "+
			"startReconnectLoop in failback_integration_test.go — the latter is behind "+
			"the `integration` build tag and will NOT fail alongside this test. Each "+
			"literal must either supply the field or leave it zero because that path "+
			"is knowingly not exercised.", got, want)
	}
}
