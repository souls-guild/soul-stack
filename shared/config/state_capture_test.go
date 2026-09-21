package config

import (
	"testing"
)

// TestHasStateCapture — ★ GUARD for the Passage-boundary state re-read ([ADR-0084]).
// HasStateCapture is the gate on that re-read in the Runner's Passage loop: true →
// `incarnation.state` is re-read from Postgres before re-rendering Passage p>0, so a
// capture from an earlier Passage is visible; false → renderIn keeps the ONE pre-run
// snapshot and the render stays bit-for-bit what it was before this ADR.
//
// Both directions matter. A false negative silently reverts accumulation to the frozen
// snapshot §5 of ADR-0083 used to mandate; a false positive puts an unnecessary SELECT
// (and a new abort path) on every staged run that never touches state.
func TestHasStateCapture(t *testing.T) {
	tasks := func(t *testing.T, src string) []Task {
		t.Helper()
		m, _, _, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if err != nil {
			t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
		}
		return m.Tasks
	}

	// Every verb of the family, one per address (ADR-057 grammar ported by ADR-0084).
	// Table-driven over the address so a NEW verb added to the family without a line
	// here is visible as an omission rather than as silently-uncovered behavior.
	verbs := map[string]string{
		"set":     "      field: endpoint\n      value: \"10.0.0.1\"\n",
		"present": "      field: endpoint\n      value: \"10.0.0.1\"\n",
		"add":     "      field: members\n      key: id\n      value: {\"id\": \"m1\"}\n",
		"append":  "      field: members\n      value: \"m1\"\n",
		"modify":  "      field: members\n      match: {\"id\": \"m1\"}\n      patch: {\"role\": \"replica\"}\n",
		"remove":  "      field: members\n      match: {\"id\": \"m1\"}\n",
		"unset":   "      field: seeded_from\n",
	}
	for verb, params := range verbs {
		t.Run("verb-"+verb, func(t *testing.T) {
			src := `
name: create
tasks:
  - name: Capture
    module: core.state.` + verb + `
    params:
` + params
			if !HasStateCapture(tasks(t, src)) {
				t.Errorf("HasStateCapture(core.state.%s) = false, want true", verb)
			}
		})
	}

	// A capture nested in a block: — a block is an atomic Passage unit, so a capture
	// inside one still moves incarnation.state mid-run. Recursion, symmetric with
	// taskHasRefreshEmitter.
	const inBlock = `
name: create
tasks:
  - name: Provision then capture
    block:
      - name: Provision
        module: core.exec.run
        changed_when: false
        params:
          cmd: "echo provision"
      - name: Capture the endpoint
        module: core.state.set
        params:
          field: endpoint
          value: "10.0.0.1"
`
	if !HasStateCapture(tasks(t, inBlock)) {
		t.Error("HasStateCapture(capture inside block:) = false, want true")
	}

	// ★ REVERSE: a plan with no capture. Nothing here can move the row, so the
	// Passage loop must NOT re-read — including `core.soul.registered`, whose base
	// shares the `core.` prefix but not the module.
	const noCapture = `
name: create
tasks:
  - name: Register and refresh
    module: core.soul.registered
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
  - name: Deploy role
    module: core.exec.run
    changed_when: false
    params:
      cmd: "redis-server"
`
	if HasStateCapture(tasks(t, noCapture)) {
		t.Error("HasStateCapture(plan without a capture) = true, want false")
	}
	if HasStateCapture(nil) {
		t.Error("HasStateCapture(nil) = true, want false")
	}

	// ★ REVERSE, prefix boundary: the match is on the module BASE, not on a string
	// prefix. A hypothetical `core.stateful.set` shares the first ten characters of
	// `core.state.` and must not be mistaken for a capture. Built as a Task directly
	// — the manifest loader rejects an unknown core module, and the point of the
	// case is the predicate, not the loader.
	notAState := []Task{{
		Name:   "Impostor",
		Module: &ModuleTask{Module: "core.stateful.set"},
	}}
	if HasStateCapture(notAState) {
		t.Error("HasStateCapture(core.stateful.set) = true, want false")
	}
}
