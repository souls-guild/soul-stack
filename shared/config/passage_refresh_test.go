package config

import (
	"testing"
)

// Guard tests S2 (ADR-0061 §S2): `refresh_soulprint: true` on core.soul.registered
// makes the step a PASSAGE-DEFINING "roster-refreshed" emitter. Any subsequent
// roster consumer (soulprint.hosts / on:[incarnation.name] / soulprint.self.* /
// omitted on:) MUST move to a Passage STRICTLY AFTER the refresh step — otherwise it
// renders against the OLD (pre-growth) roster = silent-wrong-target on a destructive
// operation (★ BLOCKER ADR-056 §risks: redis-apply on an empty/incomplete set).

// --- fixtures provision→refresh→role (target scenario ADR-0061) ---

// refreshThenSoulprintHosts — Passage 0: cloud-provision (keeper) + refresh step;
// Passage 1: a task reading soulprint.hosts in an assert. The refresh step pushes the
// soulprint.hosts consumer into the next Passage.
const refreshThenSoulprintHosts = `
name: create
state_changes: {}
tasks:
  - name: Register and await created hosts
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply redis role to grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`

// refreshThenOnIncarnation — refresh step (keeper) + a task with on:[incarnation.name]
// (role over the whole grown roster). on:[incarnation.name] is roster targeting,
// resolved from in.Hosts: after refresh it must see the new SIDs → next Passage.
const refreshThenOnIncarnation = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to all incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server --start"
`

// refreshThenSoulprintSelf — refresh step + a task reading soulprint.self.* in
// where:. soulprint.self is host-variant (depends on which hosts are in the roster),
// so this is also a roster read → next Passage.
const refreshThenSoulprintSelf = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Configure each host by its facts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    where: "soulprint.self.os.family == 'debian'"
    changed_when: false
    params:
      cmd: "apt-get update"
`

// refreshThenOmittedOn — refresh step + a task with an OMITTED on: (= the whole
// incarnation, the whole roster). An omitted on: is also roster targeting
// (orchestration.md §3: omitted on: = the whole incarnation), so after refresh it
// must move to the next Passage, else it runs on the old roster.
const refreshThenOmittedOn = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply baseline to whole incarnation
    module: core.exec.run
    changed_when: false
    params:
      cmd: "baseline"
`

// TestStratify_RefreshThenSoulprintHosts — ★ PRIMARY CASE S2 (ADR-0061). refresh step
// (Passage 0) + soulprint.hosts consumer → DIFFERENT Passages (consumer in Passage 1).
func TestStratify_RefreshThenSoulprintHosts(t *testing.T) {
	p := stratify(t, refreshThenSoulprintHosts)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh boundary splits the refresh step and the soulprint.hosts consumer)", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("refresh-step passage = %d, want 0", p.TaskPassage[0])
	}
	if p.TaskPassage[1] != 1 {
		t.Errorf("soulprint.hosts consumer passage = %d, want 1 (STRICTLY after refresh)", p.TaskPassage[1])
	}
}

// TestStratify_RefreshThenOnIncarnation — refresh step + on:[incarnation.name]
// (roster targeting) → different Passages. Without this, redis-apply would land in
// the same Passage with the old (empty) roster = silent-wrong-target.
func TestStratify_RefreshThenOnIncarnation(t *testing.T) {
	p := stratify(t, refreshThenOnIncarnation)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh boundary splits the refresh step and the on:[incarnation.name] consumer)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (on:[incarnation.name] STRICTLY after refresh)", p.TaskPassage)
	}
}

// TestStratify_RefreshThenSoulprintSelf — refresh step + soulprint.self.* in where:
// (host-variant read) → different Passages.
func TestStratify_RefreshThenSoulprintSelf(t *testing.T) {
	p := stratify(t, refreshThenSoulprintSelf)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh boundary splits the refresh step and the soulprint.self consumer)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (soulprint.self STRICTLY after refresh)", p.TaskPassage)
	}
}

// TestStratify_RefreshThenOmittedOn — refresh step + a task with an OMITTED on: (the
// whole incarnation = the whole roster) → different Passages. An omitted on: is
// roster targeting, activated as a refresh consumer ONLY when a refresh emitter is in
// the plan.
func TestStratify_RefreshThenOmittedOn(t *testing.T) {
	p := stratify(t, refreshThenOmittedOn)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (omitted on: after refresh = the whole grown roster -> next Passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (omitted on: STRICTLY after refresh)", p.TaskPassage)
	}
}

// TestStratify_NoRefreshConsumerSamePassage — ★ CONTROL (typo/missing refresh). The
// same plan but WITHOUT refresh_soulprint: true on the registering step. The roster
// consumer need not split along the roster axis → both in Passage 0 (no refresh
// boundary). A "refresh boundary active without the flag" regression reddens this
// test: the roster consumer would falsely move to Passage 1.
func TestStratify_NoRefreshConsumerSamePassage(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register hosts WITHOUT refresh
    module: core.soul.registered
    on: keeper
    params:
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 -- without refresh_soulprint the roster boundary is NOT active (typo/missing flag -> consumer in the same Passage)", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (no refresh emitter -> one Passage)", i, pass)
		}
	}
}

// TestStratify_RefreshFalseNotEmitter — refresh_soulprint: false (explicit false)
// does NOT make the step a refresh emitter → roster consumer in the same Passage.
// Catches the "any refresh_soulprint key = emitter" regression (must be exactly true).
func TestStratify_RefreshFalseNotEmitter(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register hosts with refresh disabled
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: false
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "role"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 -- refresh_soulprint: false is NOT an emitter", p.Count)
	}
}

// TestStratify_RefreshBeforeAndAfterRoster — refresh step BETWEEN two roster
// consumers: the first (BEFORE refresh) in Passage 0, the second (AFTER refresh) in
// Passage 1. Proves the refresh boundary works by program order: only SUBSEQUENT
// consumers split, preceding ones don't.
func TestStratify_RefreshBeforeAndAfterRoster(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Act on initial roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "initial"
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Act on grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "grown"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2", p.Count)
	}
	want := []int{0, 0, 1}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("task #%d passage = %d, want %d (refresh boundary: only SUBSEQUENT roster consumers are split)", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_RefreshThenAssertTopology — a topology assert (soulprint.hosts in
// that[]) AFTER the refresh step → Passage 1. An assert is evaluated Keeper-side at
// render (like where:), so it must see the grown roster (ADR-0061 §determinism).
func TestStratify_RefreshThenAssertTopology(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Assert the grown topology
    assert:
      that:
        - "size(soulprint.hosts) == 3"
      message: "expected 3 hosts after provisioning"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (assert soulprint.hosts AFTER refresh -> next Passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (assert topology sees the grown roster)", p.TaskPassage)
	}
}

// TestStratify_RefreshIsRosterAxisNotRegisterEdges — ★ AXIS INVARIANT (ADR-0061 §S2):
// the refresh boundary is a SEPARATE axis from register; it does NOT introduce
// cross-task register edges. Proven directly by set comparison: on a refresh fixture
// where the Passage split is caused SOLELY by the roster boundary (a soulprint.hosts
// consumer after the refresh step), the set of passage-defining cross-task
// register-reads (taskRegisterReads/collectTaskReads) is EMPTY for EVERY task — i.e.
// Count==2 comes from the roster axis, not the register graph. A "refresh logic
// leaked into register-reads" regression (e.g. the refresh emitter wrongly becoming a
// register-edge source) reddens this test: a non-empty read-set / register edge
// appears.
//
// reads⊆refs (ADR-056) holds trivially here (∅ ⊆ refs): refresh adds no register
// references, so the existing register invariant is untouched.
func TestStratify_RefreshIsRosterAxisNotRegisterEdges(t *testing.T) {
	// refresh step (register: roster — its own register, not cross-task) +
	// roster consumer (soulprint.hosts). No cross-task register references: the split
	// must come ONLY from the roster boundary.
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register a fixed host and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			t.Fatalf("false unknown_register_reference (%s): the refresh boundary must not introduce register references", d.Message)
		}
	}

	// ★ Direct set comparison: no task has cross-task register-reads. The set of
	// passage-defining register-reads is EMPTY on both tasks — so there are no
	// register edges, and the split is entirely on the roster axis.
	emitter := emitterIndex(m.Tasks)
	for i := range m.Tasks {
		reads := taskRegisterReads(&m.Tasks[i])
		if len(reads) != 0 {
			t.Errorf("task #%d has cross-task register-reads %v — the refresh fixture must split ONLY on the roster boundary (there must be no register edges)", i, reads)
		}
		// reads⊆refs: every read register must have an emitter (here reads is empty,
		// the check is trivial, but it pins the invariant against regression).
		for _, name := range reads {
			if _, ok := emitter[name]; !ok {
				t.Errorf("task #%d reads register %q with no emitter — reads⊄refs", i, name)
			}
		}
	}

	// Stratify does not fail (register graph is clean) and yields exactly the roster split.
	p, serr := Stratify(m.Tasks)
	if serr != nil {
		t.Fatalf("Stratify: %v (the refresh boundary must not break a clean register graph)", serr)
	}
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (split on the roster axis)", p.Count)
	}
	// Without the refresh boundary (refreshEmitters empty) the same plan would not
	// split: a control proof that Count==2 is owed to the roster axis.
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (roster consumer strictly after refresh)", p.TaskPassage)
	}
}

// TestHasRefreshEmitter — detector for "the plan provisions the roster mid-run"
// (ADR-0061 amendment, no_hosts-bypass class (b)). true ONLY for core.soul.registered
// with a literal refresh_soulprint: true; adjacent forms (no flag / false / another
// keeper module / empty plan) → false. A pure function over a flat plan.
func TestHasRefreshEmitter(t *testing.T) {
	tasks := func(t *testing.T, src string) []Task {
		t.Helper()
		m, _, _, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if err != nil {
			t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
		}
		return m.Tasks
	}

	// Baseline: refresh emitter + a host deploy task (the target mixed bypass plan).
	const mixedWithRefresh = `
name: create
state_changes: {}
tasks:
  - name: Provision and refresh
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "${ register.provision.hosts }"
      coven: ["${ incarnation.name }"]
  - name: Deploy role
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server"
`
	const noFlag = `
name: create
state_changes: {}
tasks:
  - name: Register without refresh
    module: core.soul.registered
    on: keeper
    params:
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
`
	const refreshFalse = `
name: create
state_changes: {}
tasks:
  - name: Register with refresh disabled
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: false
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
`
	// Another keeper module with a same-named param — NOT an emitter (the carrier
	// module is only core.soul.registered).
	const otherKeeperModule = `
name: create
state_changes: {}
tasks:
  - name: Cloud provision
    module: core.cloud.created
    on: keeper
    params:
      refresh_soulprint: true
      profile: prod
`
	const hostOnly = `
name: create
state_changes: {}
tasks:
  - name: Deploy role
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server"
`
	// A refresh emitter nested in block: — recognized recursively.
	const refreshInBlock = `
name: create
state_changes: {}
tasks:
  - name: Provision group
    block:
      - name: Register and refresh
        module: core.soul.registered
        on: keeper
        params:
          refresh_soulprint: true
          sid: "host-a.example.com"
          coven: ["${ incarnation.name }"]
`

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"mixed-with-refresh", mixedWithRefresh, true},
		{"refresh-in-block", refreshInBlock, true},
		{"no-flag", noFlag, false},
		{"refresh-false", refreshFalse, false},
		{"other-keeper-module", otherKeeperModule, false},
		{"host-only", hostOnly, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRefreshEmitter(tasks(t, tt.src)); got != tt.want {
				t.Errorf("HasRefreshEmitter = %v, want %v", got, tt.want)
			}
		})
	}

	// Empty plan — separately (LoadScenarioManifestFromBytes requires tasks non-empty,
	// so build the slice directly).
	if HasRefreshEmitter(nil) {
		t.Error("HasRefreshEmitter(nil) = true, want false")
	}
	if HasRefreshEmitter([]Task{}) {
		t.Error("HasRefreshEmitter([]) = true, want false")
	}
}
