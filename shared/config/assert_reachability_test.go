package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard tests for [validateAssertReachability] (NIM-272): the linter names, at
// authoring time, the one place where a topology `assert:` does NOT answer the
// way the ADR-009 amendment promises — a create starter, whose gate runs before
// the incarnation row exists and therefore before it has any roster.
//
// The boundary is what these pin. Warn too widely and every size-guard in the
// tree grows a permanent warning nobody can act on; warn too narrowly and the
// author learns the difference from a live stand.

const assertReachCode = "assert_roster_deferred_on_create"

// scenarioWithAssert builds a one-assert scenario. create controls the
// `create: true` line, that is the predicate under test.
func scenarioWithAssert(create bool, when, predicate string) string {
	var b strings.Builder
	b.WriteString("name: create\n")
	if create {
		b.WriteString("create: true\n")
	}
	b.WriteString("tasks:\n  - name: Guard\n")
	if when != "" {
		b.WriteString("    when: " + when + "\n")
	}
	b.WriteString("    assert:\n      that:\n        - " + predicate + "\n      message: \"nope\"\n")
	return b.String()
}

func TestAssertReachability(t *testing.T) {
	const rosterPredicate = `"size(soulprint.hosts) == int(input.shards)"`
	const inputPredicate = `"int(input.replicas) <= int(input.max_replicas)"`

	tests := []struct {
		name string
		src  string
		warn bool
	}{
		// The shape the warning exists for: a create starter guarding its
		// topology. Legal, and deferred to render on the create path.
		{"create-starter-roster-assert", scenarioWithAssert(true, "", rosterPredicate), true},

		// A create starter whose assert reads only input: answered pre-flight
		// on every path, nothing to warn about. Warning here would tell authors
		// to stop using the mechanism that still works.
		{"create-starter-input-assert", scenarioWithAssert(true, "", inputPredicate), false},

		// Not a create starter: the scenario is only ever reached as an
		// explicit run, where the gate DOES answer the roster assert (NIM-270).
		{"day2-roster-assert", scenarioWithAssert(false, "", rosterPredicate), false},

		// The roster read hidden in the `when:` gate rather than the predicate —
		// the gate decision is roster-dependent, so the whole assert defers.
		// Same conservative direction as config.AssertReadsRoster.
		{"create-starter-roster-in-when", scenarioWithAssert(true, `"size(soulprint.hosts) > 0"`, inputPredicate), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(tt.src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			got := hasCode(diags, assertReachCode)
			if got != tt.warn {
				dump(t, diags)
				t.Fatalf("%s = %v, want %v", assertReachCode, got, tt.warn)
			}
			if !got {
				return
			}
			// WARNING, never ERROR: the construct is legal and `make lint` must
			// stay green over examples/ that legitimately use it.
			for _, d := range diags {
				if d.Code == assertReachCode && d.Level != diag.LevelWarning {
					t.Errorf("%s level = %v, want warning — a legal construct must not fail the gate", assertReachCode, d.Level)
				}
			}
		})
	}
}

// TestAssertReachability_PointsAtTheAssert — the diagnostic has to land on the
// offending task, not on the document: a scenario carries many tasks and the
// author needs to know which guard is being talked about.
func TestAssertReachability_PointsAtTheAssert(t *testing.T) {
	src := `name: create
create: true
tasks:
  - name: Echo
    module: core.exec.run
    changed_when: "false"
    params:
      cmd: echo
      args: ["hi"]
  - name: Guard roster
    assert:
      that:
        - "size(soulprint.hosts) == 3"
      message: "nope"
`
	_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	var found *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == assertReachCode {
			found = &diags[i]
			break
		}
	}
	if found == nil {
		dump(t, diags)
		t.Fatal("expected the warning on the second task")
	}
	if found.YAMLPath != "$.tasks[1].assert" {
		t.Errorf("YAMLPath = %q, want $.tasks[1].assert", found.YAMLPath)
	}
	if found.Line == 0 {
		t.Error("diagnostic carries no line — the author cannot find the guard it means")
	}
	// The hint must offer the alternative, not merely state the deferral:
	// an input-only invariant belongs in validate:, which answers on both paths.
	if !strings.Contains(found.Hint, "validate:") {
		t.Errorf("hint does not point at validate: %q", found.Hint)
	}
}
