package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// loadTasks parses inline scenario YAML into []Task (after the common config
// validation), failing on any error diagnostic. Guard-test fixtures are inline, NOT
// loaded from examples/**: examples are the user's WIP zone (uncommitted edits), and
// the silent-wrong-target guard invariant must be deterministic and independent of
// the examples' state. The fixtures below are synthetic, reproducing the 3-Passage
// re-probe idiom (historically redis-cluster restart).
func loadTasks(t *testing.T, src string) []Task {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	return m.Tasks
}

func stratify(t *testing.T, src string) Passage {
	t.Helper()
	p, err := Stratify(loadTasks(t, src))
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	return p
}

// --- synthetic 3-Passage re-probe fixtures (historically redis-cluster restart) ---

const redisUpdateACL = `
name: update_acl
input:
  changes:
    type: object
    required: true
    properties: {}
    additional_properties:
      type: object
      properties:
        acl: { type: string }
      required: [acl]
state_changes:
  modifies: [redis_users.*.acl]
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Diff and apply ACL changes on the current master
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action:  update_acls
        changes: "${ input.changes }"
`

const redisAddUser = `
name: add_user
input:
  user: { type: string, required: true }
  acl:  { type: string, required: true }
  state: { type: string, required: true, enum: [on, off] }
state_changes:
  appends: [redis_users]
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Create the user on the current master
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action: ensure_user
        user:   "${ input.user }"
  - name: Wait until the user is replicated to all replicas
    module: core.exec.run
    where: register.redis_role.stdout == 'slave'
    changed_when: false
    retry:
      count: 15
      delay: 2s
      until: contains(register.self.stdout, input.user)
    failed_when: "!contains(register.self.stdout, input.user)"
    params:
      cmd: redis-cli
      args: ["ACL", "GETUSER", "${ input.user }"]
`

const redisRestart = `
name: restart
input:
  reason: { type: string, default: "manual restart" }
state_changes: {}
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Rolling-restart replicas one at a time
    where: register.redis_role.stdout == 'slave'
    serial: 1
    block:
      - name: Restart redis-server
        module: core.service.restarted
        params:
          name: redis-server
      - name: Wait until replica is healthy again
        module: core.exec.run
        changed_when: false
        retry:
          count: 12
          delay: 5s
          until: contains(register.self.stdout, 'master_link_status:up')
        failed_when: "!contains(register.self.stdout, 'master_link_status:up')"
        params:
          cmd: redis-cli
          args: ["INFO", "replication"]
  - name: Failover and restart the current master
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action: failover_and_restart
  - name: Re-detect redis role after failover
    module: core.cmd.shell
    register: redis_role_after
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Restart the former master (now a replica)
    where: register.redis_role_after.stdout == 'slave' && register.redis_role.stdout == 'master'
    apply:
      destiny: redis
      input:
        action: restart
`

// TestStratify_RedisUpdateACL — a real 2-Passage scenario: probe (p0) → task with
// where: register.redis_role (p1). One probe barrier.
func TestStratify_RedisUpdateACL(t *testing.T) {
	p := stratify(t, redisUpdateACL)
	if p.Count != 2 {
		t.Fatalf("update_acl: Count = %d, want 2", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("probe task #0 passage = %d, want 0", p.TaskPassage[0])
	}
	if p.TaskPassage[1] != 1 {
		t.Errorf("where-task #1 passage = %d, want 1 (STRICTLY after probe)", p.TaskPassage[1])
	}
}

// TestStratify_RedisAddUser — 2-Passage: probe (p0) → two tasks on p1 (create on
// master + health-gate on slave, both read register.redis_role). The health-gate
// reads register.self in until/failed_when — that is NOT a cross-task edge, it does
// not raise the passage.
func TestStratify_RedisAddUser(t *testing.T) {
	p := stratify(t, redisAddUser)
	if p.Count != 2 {
		t.Fatalf("add_user: Count = %d, want 2", p.Count)
	}
	want := []int{0, 1, 1}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("add_user task #%d passage = %d, want %d", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_RedisRestart — the main 3-Passage case (ADR-056 §"restart re-probe"):
// probe(p0) → where-tasks(p1) → re-probe(p1, re-measured AFTER failover) →
// where: register.redis_role_after && register.redis_role (p2). Two probe boundaries
// → three Passages. This is "probe → act → re-probe → act".
func TestStratify_RedisRestart(t *testing.T) {
	p := stratify(t, redisRestart)
	if p.Count != 3 {
		t.Fatalf("restart: Count = %d, want 3 (two probe boundaries)", p.Count)
	}
	// #0 probe; #1 rolling-restart(where slave); #2 failover(where master);
	// #3 re-probe; #4 restart former master(where redis_role_after && redis_role).
	want := []int{0, 1, 1, 1, 2}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("restart task #%d passage = %d, want %d", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_InvariantConsumerStrictlyAfterProbe — the MAIN guard invariant
// (security-critical, ADR-056 silent-wrong-target). For EVERY task that reads
// register.X, its passage MUST be STRICTLY greater than the passage of the probe
// that emits X. A regression sending the consumer to <= the probe's passage means
// resolving where: over an empty/stale register → a destructive operation on an
// unresolved target SILENTLY. Checked on all synthetic fixtures via a direct graph walk.
func TestStratify_InvariantConsumerStrictlyAfterProbe(t *testing.T) {
	fixtures := map[string]string{
		"update_acl": redisUpdateACL,
		"add_user":   redisAddUser,
		"restart":    redisRestart,
	}
	for name, src := range fixtures {
		t.Run(name, func(t *testing.T) {
			tasks := loadTasks(t, src)
			p, err := Stratify(tasks)
			if err != nil {
				t.Fatalf("Stratify: %v", err)
			}
			emitter := emitterIndex(tasks)
			for i := range tasks {
				for _, x := range taskRegisterReads(&tasks[i]) {
					src, ok := emitter[x]
					if !ok {
						t.Fatalf("task #%d reads register %q with no emitter — fixture broken", i, x)
					}
					if p.TaskPassage[i] <= p.TaskPassage[src] {
						t.Errorf("INVARIANT VIOLATED: task #%d (passage %d) reads register %q emitted by task #%d (passage %d) — consumer must be STRICTLY after probe, else silent-wrong-target",
							i, p.TaskPassage[i], x, src, p.TaskPassage[src])
					}
				}
			}
		})
	}
}

// TestStratify_BackwardCompatNoRegister — a scenario WITHOUT cross-task register:
// each task reads only input/own register → all passages 0, Count==1 (fast-path,
// identical to the current up-front render).
func TestStratify_BackwardCompatNoRegister(t *testing.T) {
	const src = `
name: create
input:
  pkg: { type: string, required: true }
tasks:
  - name: Install package
    module: core.pkg.installed
    params:
      name: "${ input.pkg }"
  - name: Start service
    module: core.service.running
    params:
      name: redis-server
  - name: Probe without consumers
    module: core.cmd.shell
    register: anything
    changed_when: false
    failed_when: "register.self.rc != 0"
    params:
      cmd: "true"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("no-register scenario: Count = %d, want 1 (fast-path)", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (no cross-task register)", i, pass)
		}
	}
}

// TestStratify_Cycle — a circular register dependency → an explicit StratifyCycle
// error, NOT silent stratification. probe_a reads register.b, probe_b reads register.a.
func TestStratify_Cycle(t *testing.T) {
	const src = `
name: cyclic
tasks:
  - name: Probe A reads B
    module: core.cmd.shell
    register: a
    changed_when: false
    where: register.b.stdout == 'x'
    params: { cmd: "true" }
  - name: Probe B reads A
    module: core.cmd.shell
    register: b
    changed_when: false
    where: register.a.stdout == 'y'
    params: { cmd: "true" }
`
	_, err := Stratify(loadTasks(t, src))
	if err == nil {
		t.Fatal("Stratify: expected cycle error, got nil")
	}
	var se *StratifyError
	if !errors.As(err, &se) || se.Code != StratifyCycle {
		t.Fatalf("Stratify: error code = %v, want %s", err, StratifyCycle)
	}
}

// TestStratify_UnknownRegister — a task reads register.X but NO task emits it. A
// double line of defense:
//
//  1. CONFIRMS the existing validator: config parse ALREADY raises
//     unknown_register_reference (cross-ref phase, task_refs.go) — the first line.
//  2. Render-layer safety net: even if tasks reach Stratify (e.g. validation
//     disabled/skipped), stratification fails explicitly with StratifyUnknownRegister
//     instead of walking an incomplete graph → silent-wrong-target.
func TestStratify_UnknownRegister(t *testing.T) {
	const src = `
name: dangling
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "true" }
  - name: Act on a register nobody emits
    where: register.ghost_role.stdout == 'master'
    apply:
      destiny: redis
      input: { action: noop }
`
	// Defense 1: the config validator catches the dangling reference at parse.
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	foundDiag := false
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			foundDiag = true
		}
	}
	if !foundDiag {
		t.Error("existing config-validator did NOT raise unknown_register_reference — render-side Stratify is now the only guard")
	}

	// Defense 2: Stratify fails explicitly, not silently.
	_, serr := Stratify(m.Tasks)
	if serr == nil {
		t.Fatal("Stratify: expected unknown-register error, got nil")
	}
	var se *StratifyError
	if !errors.As(serr, &se) || se.Code != StratifyUnknownRegister {
		t.Fatalf("Stratify: error code = %v, want %s", serr, StratifyUnknownRegister)
	}
}

// TestStratify_RegisterInParamsAndInput — a cross-task register threaded through
// ${ … } in params: and apply:input: (not only where:) also moves the passage.
// Catches a regression where stratification looks ONLY at where: and misses a
// register in the next task's data (which also needs a probe barrier before render).
func TestStratify_RegisterInParamsAndInput(t *testing.T) {
	const src = `
name: chain
tasks:
  - name: Probe value
    module: core.cmd.shell
    register: probe
    changed_when: false
    params: { cmd: "true" }
  - name: Use register in params
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ register.probe.stdout }"]
  - name: Use register in apply input
    apply:
      destiny: redis
      input:
        seed: "${ register.probe.stdout }"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 || p.TaskPassage[2] != 1 {
		t.Fatalf("passages = %v, want [0 1 1] (register in params/input moves passage)", p.TaskPassage)
	}
}

// TestStratify_SelfRegisterNotCrossTask — a task with register: probe that reads
// register.self.* AND its own named register (redis_role in its own probe's
// failed_when) does NOT depend on itself: it stays passage 0. Catches a regression
// where an own/self reference is wrongly treated as a cross-task edge (would give a
// cycle/shift).
func TestStratify_SelfRegisterNotCrossTask(t *testing.T) {
	const src = `
name: self
tasks:
  - name: Probe with self-referential predicate
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    failed_when: size(register.redis_role) < incarnation.host_count
    retry:
      count: 3
      delay: 1s
      until: contains(register.self.stdout, 'ok')
    params: { cmd: "true" }
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 (self/own register is not cross-task)", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("probe passage = %d, want 0", p.TaskPassage[0])
	}
}

// TestStratify_RegisterInOutput — a cross-task register threaded through ${ … } in
// `output:` (a destiny/scenario task's declared output, read by the consumer via
// register:) also moves the passage. output is a passage-defining source (ADR-056
// registry); a regression where collectTaskReads skips it would leave the
// output-register consumer in the same Passage as the probe → silent-wrong-target.
func TestStratify_RegisterInOutput(t *testing.T) {
	const src = `
name: chain_output
tasks:
  - name: Probe value
    module: core.cmd.shell
    register: probe
    changed_when: false
    params: { cmd: "true" }
  - name: Expose probe result via output
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.probe.stdout }"
    params: { cmd: "true" }
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (register in output moves passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (output consumer STRICTLY after probe)", p.TaskPassage)
	}
}

// registerSourceFields — the canonical registry of passage-defining register
// sources of a Task (ADR-056), EACH as a minimal scenario fixture where a
// ghost-register appears ONLY in that field and NOBODY emits it. Key — the
// human-readable field name; value — the whole scenario's YAML.
//
// This is the reads==refs consistency mechanism: both register-reference graphs —
// the stratifier's (render.collectTaskReads → Stratify) and the config validator's
// (config.collectRefs → unknown_register_reference) — MUST catch the ghost in every
// field. If someone adds a new register-reading source-field to one walker but
// forgets the other (or removes it from one), the matching sub-test goes red: either
// Stratify does not return StratifyUnknownRegister (the stratifier does not see the
// field → a silently incomplete graph → silent-wrong-target), or the config
// validator does not raise unknown_register_reference (a linter hole → unknown
// survives to runtime). requisites (onchanges/onfail/require) and flow-control
// (when/...) are NOT included here — they are NOT passage-defining (see ADR-056 §registry).
var registerSourceFields = map[string]string{
	"where": `
name: f_where
tasks:
  - name: consumer
    module: core.exec.run
    where: register.ghost.stdout == 'x'
    changed_when: false
    params: { cmd: "true" }
`,
	"vars": `
name: f_vars
tasks:
  - name: consumer
    module: core.exec.run
    vars:
      v: "${ register.ghost.stdout }"
    changed_when: false
    params: { cmd: "true" }
`,
	"params": `
name: f_params
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    params:
      cmd: echo
      args: ["${ register.ghost.stdout }"]
`,
	"apply.input": `
name: f_apply_input
tasks:
  - name: consumer
    apply:
      destiny: redis
      input:
        seed: "${ register.ghost.stdout }"
`,
	"output": `
name: f_output
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.ghost.stdout }"
    params: { cmd: "true" }
`,
	"loop.items": `
name: f_loop_items
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    loop:
      items: "${ register.ghost.stdout }"
      as: item
    params: { cmd: "echo ${ item }" }
`,
	"block": `
name: f_block
tasks:
  - name: container
    block:
      - name: nested consumer
        module: core.exec.run
        where: register.ghost.stdout == 'x'
        changed_when: false
        params: { cmd: "true" }
`,
}

// TestStratify_ReadsEqRefsConsistency — ★ guard against silently blurring the
// register graph: the set of PASSAGE-DEFINING source-fields covered by the
// stratifier (collectTaskReads) MUST match those covered by the config validator
// (collectRefs) in this class. For each source-field (where / vars / params /
// apply.input / output / loop.items / block) the ghost-register must be caught by
// BOTH: Stratify → StratifyUnknownRegister AND config validator →
// unknown_register_reference.
//
// Flow-control (when/changed_when/failed_when) is NOT included here — it ∈
// collectRefs but ∉ collectTaskReads (deliberate asymmetry after the FC-5
// narrow-fix; see TestStratify_FlowControlInRefsNotPassageReads below). A
// passage-defining-class field added to one walker but not the other reddens exactly
// the sub-test matching the divergence — this is the ADR-056 maintenance invariant.
func TestStratify_ReadsEqRefsConsistency(t *testing.T) {
	for field, src := range registerSourceFields {
		t.Run(field, func(t *testing.T) {
			// Validator side: unknown_register_reference at parse.
			m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			// No OTHER error diagnostics (the fixture is structurally valid except the
			// expected unknown_register) — else the test is green for the wrong reason.
			foundUnknown := false
			for _, d := range diags {
				if d.Level != diag.LevelError {
					continue
				}
				if d.Code == "unknown_register_reference" {
					foundUnknown = true
					continue
				}
				t.Fatalf("unexpected error diagnostic (%s): %s — fixture for %q is structurally broken", d.Code, d.Message, field)
			}
			if !foundUnknown {
				t.Errorf("config-validator (collectRefs) did NOT raise unknown_register_reference for ghost in %q — source-field coverage diverged from stratifier", field)
			}

			// Stratifier side: StratifyUnknownRegister on the same ghost.
			_, serr := Stratify(m.Tasks)
			if serr == nil {
				t.Fatalf("Stratify did NOT fail for ghost in %q — collectTaskReads does not cover this source-field (silent-wrong-target risk)", field)
			}
			var se *StratifyError
			if !errors.As(serr, &se) || se.Code != StratifyUnknownRegister {
				t.Fatalf("Stratify for %q: error code = %v, want %s", field, serr, StratifyUnknownRegister)
			}
		})
	}
}

// flowControlFields — flow-control source-fields (when / changed_when / failed_when),
// EACH as a minimal fixture where a ghost-register appears ONLY in that field and
// NOBODY emits it. After the FC-5 narrow-fix flow-control is NOT passage-defining
// (ADR-056:85) but REMAINS register-reading (the cross-ref validator checks the
// register exists). This pins the asymmetry: refs ⊋ passage-reads.
var flowControlFields = map[string]string{
	"when": `
name: fc_when
tasks:
  - name: consumer
    module: core.exec.run
    when: register.ghost.stdout == 'x'
    changed_when: false
    params: { cmd: "true" }
`,
	"changed_when": `
name: fc_changed_when
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: register.ghost.rc == 0
    params: { cmd: "true" }
`,
	"failed_when": `
name: fc_failed_when
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    failed_when: register.ghost.rc != 0
    params: { cmd: "true" }
`,
}

// TestStratify_FlowControlInRefsNotPassageReads — ★ guard pinning the FC-5 asymmetry
// (ADR-056:85 amend): flow-control `when`/`changed_when`/`failed_when` is
// register-READING (∈ collectRefs → cross-ref validator catches the ghost) but NOT
// passage-DEFINING (∉ collectTaskReads → Stratify does NOT fail
// StratifyUnknownRegister and does NOT build a passage edge from it).
//
// Before the narrow-fix, flow-control was in collectTaskReads (conservative
// over-approx) and split a probe↔same-passage-when-consumer → Soul did not see the
// cross-passage register → `no such key` (FC-5). A regression "return flow-control to
// collectTaskReads" reddens this test: Stratify would start failing on a
// ghost-register in a flow-control field.
func TestStratify_FlowControlInRefsNotPassageReads(t *testing.T) {
	for field, src := range flowControlFields {
		t.Run(field, func(t *testing.T) {
			m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			// (a) ∈ refs: the cross-ref validator MUST catch the ghost (register-reading field).
			foundUnknown := false
			for _, d := range diags {
				if d.Level != diag.LevelError {
					continue
				}
				if d.Code == "unknown_register_reference" {
					foundUnknown = true
					continue
				}
				t.Fatalf("unexpected error diagnostic (%s): %s — fixture for %q is structurally broken", d.Code, d.Message, field)
			}
			if !foundUnknown {
				t.Errorf("config-validator (collectRefs) did NOT raise unknown_register_reference for ghost in flow-control %q — flow-control dropped out of refs (must stay: register-reading)", field)
			}

			// (b) ∉ passage-reads: Stratify does NOT fail StratifyUnknownRegister (flow-control
			// is not in collectTaskReads → does not build a passage graph from ghost-register).
			_, serr := Stratify(m.Tasks)
			if serr != nil {
				t.Fatalf("Stratify FAILED on ghost in flow-control %q (%v) - flow-control leaked back into collectTaskReads (passage-defining), which would bring back FC-5 cross-passage splitting", field, serr)
			}
		})
	}
}

// TestStratify_RegisterDependentWhenDoesNotSplitPassage — ★ the KEY FC-5 narrow-fix
// guard (ADR-056:85). A probe emits register: redis_role (Passage 0); a task with NO
// where:/vars/params register edge of its own but with `when: register.redis_role...`
// MUST stay in the SAME Passage 0 — flow-control does not split the Passage itself.
// Then Soul sees the register same-passage → when works (as ADR-012(d) intends).
//
// Before the narrow-fix, when pushed the consumer into Passage 1 (Count=2, [0 1]) →
// cross-passage no-such-key. After — Count=1, both tasks Passage 0. A regression reddens on Count!=1.
func TestStratify_RegisterDependentWhenDoesNotSplitPassage(t *testing.T) {
	const src = `
name: when_same_passage
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: Act on master only (when-gated)
    module: core.cmd.shell
    when: register.redis_role.stdout == 'master'
    changed_when: false
    params: { cmd: "touch /tmp/acted" }
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 - a register-dependent when must NOT split Passage (flow-control is not passage-defining, ADR-056:85); before the narrow-fix it was 2 -> FC-5 cross-passage no-such-key", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (probe and when-consumer in the same Passage -> Soul sees register same-passage)", i, pass)
		}
	}
}

// TestCrossPassageWhenGating_Detect — ★ FC-5 fail-closed detection of a genuinely
// cross-passage when (ADR-056:85). probe `role` goes to Passage 0 because ANOTHER
// task targets `where: register.role` (passage-defining → the role emitter stays in
// Passage 0 by program order, the where-consumer in Passage 1). The when-task also
// went to Passage 1 by its OWN register dependency (vars: register.other) — so
// `when: register.role` is now genuinely cross-passage (role in Passage 0, consumer
// in Passage 1). The detector must reject with coordinates.
func TestCrossPassageWhenGating_Detect(t *testing.T) {
	const src = `
name: cross_passage_when
state_changes: {}
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: role
    changed_when: false
    params: { cmd: "detect-role" }
  - name: Where-target on role (forces role into Passage 0)
    module: core.exec.run
    where: register.role.stdout == 'master'
    register: other
    changed_when: false
    params: { cmd: "true" }
  - name: When-gated on role across passages
    module: core.cmd.shell
    when: register.role.stdout == 'master'
    changed_when: false
    vars:
      seed: "${ register.other.stdout }"
    params: { cmd: "true" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count <= 1 {
		t.Fatalf("expected a staged plan (Count>1), got Count=%d TaskPassage=%v", passage.Count, passage.TaskPassage)
	}
	info, bad := CrossPassageWhenGating(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageWhenGating did NOT detect a genuinely cross-passage when (register.role from Passage 0, consumer in Passage 1) - TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "when" || info.RegisterName != "role" {
		t.Errorf("info = %+v, want kind=when register=role", info)
	}
	if info.ConsumerPassage == info.SourcePassage {
		t.Errorf("consumer passage %d == source passage %d, expected them to differ", info.ConsumerPassage, info.SourcePassage)
	}
}

// TestCrossPassageWhenGating_SamePassageOK — when over a same-passage register (probe
// and when-consumer in the same Passage after the narrow-fix) → NOT rejected. A valid
// FC-5 case: Soul sees the register of its own Passage.
func TestCrossPassageWhenGating_SamePassageOK(t *testing.T) {
	const src = `
name: when_same_passage_ok
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: When-gated same passage
    module: core.cmd.shell
    when: register.redis_role.stdout == 'master'
    changed_when: false
    params: { cmd: "true" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if _, bad := CrossPassageWhenGating(tasks, passage); bad {
		t.Fatalf("CrossPassageWhenGating falsely reported a same-passage when as cross-passage - after the narrow-fix when is in the same Passage as probe (TaskPassage=%v)", passage.TaskPassage)
	}
}

// TestCrossPassageWhenGating_SelfRegisterOK — failed_when over register.self (same-task
// result, the remove_replica idiom) → NOT rejected. register.self is filtered out by
// ExtractRegisterRefs → the detector does not see it. A regression "catch register.self"
// would break a valid guard (e.g. `failed_when: register.self.stdout == 'master'`).
func TestCrossPassageWhenGating_SelfRegisterOK(t *testing.T) {
	const src = `
name: failed_when_self
tasks:
  - name: Refuse to remove the current primary
    module: core.cmd.shell
    register: role
    changed_when: false
    failed_when: register.self.stdout == 'master'
    params: { cmd: "redis-cli role | head -1 | tr -d '\n'" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if _, bad := CrossPassageWhenGating(tasks, passage); bad {
		t.Fatalf("CrossPassageWhenGating falsely reported register.self (same-task) as cross-passage - would break the remove_replica guard (TaskPassage=%v)", passage.TaskPassage)
	}
}

// TestValidate_UnknownRegisterInOutput — a closed ADR-056 S2 gap: before S2 the
// cross-ref validator did not walk interpolation source-fields, and an
// unknown-register in `output:` (like in vars/params/apply.input/loop.items) survived
// to the runtime stratifier. Now the config validator catches it OFFLINE.
func TestValidate_UnknownRegisterInOutput(t *testing.T) {
	const src = `
name: out_unknown
tasks:
  - name: Expose a register nobody emits
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.ghost.stdout }"
    params: { cmd: "true" }
`
	_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	found := false
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			found = true
		}
	}
	if !found {
		t.Error("config-validator did NOT raise unknown_register_reference for ghost in output: — the validator gap (ADR-056 S2) is not closed")
	}
}

// TestStratify_Empty — an empty task plan → Count 1, no panic.
func TestStratify_Empty(t *testing.T) {
	p, err := Stratify(nil)
	if err != nil {
		t.Fatalf("Stratify(nil): %v", err)
	}
	if p.Count != 1 || p.TaskPassage != nil {
		t.Fatalf("Stratify(nil) = %+v, want {nil, 1}", p)
	}
}

// TestCrossPassageRequisite_Detect — ★ R2 DETECTION (ADR-056 amend). A restart with
// onchanges:[cfg] is forced into Passage 1 by a SEPARATE register dependency (where:
// register.role.*); the requisite source cfg stays in Passage 0. consumer passage 1 ≠
// source passage 0 → CrossPassageRequisite catches it before dispatch. Without
// detection, Soul gating in Passage 1 does not see register cfg (a different
// ApplyRequest) → restart silently SKIPPED.
func TestCrossPassageRequisite_Detect(t *testing.T) {
	const src = `
name: cross_passage
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Apply config
    module: core.file.present
    register: cfg
    params: { path: /etc/app.conf, content: x }
  - name: Restart on master after config change
    module: core.service.restarted
    where: "register.role.stdout == 'master'"
    onchanges: [cfg]
    params: { name: app }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count <= 1 {
		t.Fatalf("expected a staged plan (Count>1), got Count=%d, TaskPassage=%v", passage.Count, passage.TaskPassage)
	}
	info, bad := CrossPassageRequisite(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageRequisite did NOT detect cross-passage onchanges (consumer/source in different Passages) - TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "onchanges" || info.RequisiteName != "cfg" {
		t.Errorf("info = %+v, want kind=onchanges requisite=cfg", info)
	}
	if info.ConsumerPassage == info.SourcePassage {
		t.Errorf("consumer passage %d == source passage %d, expected them to differ", info.ConsumerPassage, info.SourcePassage)
	}
}

// TestCrossPassageRequisite_SamePassageOK — same-passage onchanges (source and
// consumer in the same Passage, the R1-remap fixes them) → NOT rejected. N=1 without
// where: (all in Passage 0) — onchanges works normally after remap.
func TestCrossPassageRequisite_SamePassageOK(t *testing.T) {
	const src = `
name: same_passage
state_changes: {}
tasks:
  - name: Apply config
    module: core.file.present
    register: cfg
    params: { path: /etc/app.conf, content: x }
  - name: Restart on config change
    module: core.service.restarted
    onchanges: [cfg]
    params: { name: app }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count != 1 {
		t.Fatalf("expected N=1 (Count=1), got Count=%d", passage.Count)
	}
	if _, bad := CrossPassageRequisite(tasks, passage); bad {
		t.Fatalf("CrossPassageRequisite falsely reported same-passage onchanges as cross-passage - R1-remap should fix it, not reject")
	}
}

// TestCrossPassageRequisite_OnFailDetect — the onfail mirror of detection: the onfail
// source in Passage 0, the rescue task forced into Passage 1 by a where dependency →
// cross-passage reject (kind=onfail). Without detection the onfail-rescue would
// silently not run on the source's failure (Soul Passage 1 does not see the source's
// Passage 0 register).
func TestCrossPassageRequisite_OnFailDetect(t *testing.T) {
	const src = `
name: cross_passage_onfail
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Risky deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: Rollback on master after deploy fail
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    onfail: [deploy]
    params: { cmd: rollback }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	info, bad := CrossPassageRequisite(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageRequisite did NOT detect cross-passage onfail - TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "onfail" || info.RequisiteName != "deploy" {
		t.Errorf("info = %+v, want kind=onfail requisite=deploy", info)
	}
}

// --- within-block register dependency (silent-wrong-target) ---

// TestWithinBlock_PeerReject (guard #1) — ★ the MAIN CASE. A probe child emits
// register: role INSIDE a block, a sibling child of the SAME block reads
// where: register.role.* — the peer-register is unavailable at render (the block is
// atomic, the probe has not run yet). The detector must reject with exact coordinates.
func TestWithinBlock_PeerReject(t *testing.T) {
	const src = `
name: within_block_peer
state_changes: {}
tasks:
  - name: Rolling group
    block:
      - name: Probe role inside block
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Restart on master
        module: core.service.restarted
        where: "register.role.stdout == 'master'"
        params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	info, bad := WithinBlockRegisterDependency(tasks)
	if !bad {
		t.Fatalf("WithinBlockRegisterDependency did NOT detect a peer-register inside a block - silent-wrong-target would pass silently")
	}
	if info.RegisterName != "role" {
		t.Errorf("RegisterName = %q, want role", info.RegisterName)
	}
	if info.ReaderName != "Restart on master" {
		t.Errorf("ReaderName = %q, want \"Restart on master\"", info.ReaderName)
	}
	if info.EmitterName != "Probe role inside block" {
		t.Errorf("EmitterName = %q, want \"Probe role inside block\"", info.EmitterName)
	}
}

// TestWithinBlock_WhenPeerOK (guard #1b) — ★ FC-5 SIDE-EFFECT GUARD. A probe child
// emits register: role INSIDE a block, a sibling child of the SAME block gates on
// `when: register.role.stdout == 'master'` (flow-control, NOT where). After the FC-5
// narrow-fix `when` dropped out of collectTaskReads (Soul-side per-task gating) →
// within-block `when: register.peer` is VALID: the peer-probe runs in the same
// ApplyRequest BEFORE the consumer, and Soul sees the peer-register in the block's
// accumulated slice at eval time. The detector must NOT reject it → bad==false. A
// regression "return when to collectTaskReads" would silently break within-block
// when:peer — this test reddens.
func TestWithinBlock_WhenPeerOK(t *testing.T) {
	const src = `
name: within_block_when_peer
state_changes: {}
tasks:
  - name: Rolling group
    block:
      - name: Probe role inside block
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Act on master only (when-gated peer)
        module: core.cmd.shell
        when: "register.role.stdout == 'master'"
        changed_when: false
        params: { cmd: "touch /tmp/acted" }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency falsely reported within-block when:peer as silent-wrong-target (%+v) - after the FC-5 narrow-fix when dropped out of collectTaskReads, when:peer is valid (Soul-side gating, peer-probe in the same ApplyRequest)", info)
	}
}

// TestWithinBlock_ExternalProbeOK (guard #2) — ★ ACCEPTANCE NOT BROKEN. A probe emits
// register: role at TOP-LEVEL (a separate task BEFORE the block), a block child reads
// where: register.role.* — this is VALID (the probe is a separate Passage, the
// restart case). The detector checks ONLY against registers born INSIDE the block (not
// the global emitterIndex), so the external probe is not caught → ok==false. A
// regression "break restart" is pinned by this test.
func TestWithinBlock_ExternalProbeOK(t *testing.T) {
	const src = `
name: external_probe
state_changes: {}
tasks:
  - name: Probe role top-level
    module: core.cmd.shell
    register: role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: Rolling group
    where: "register.role.stdout == 'slave'"
    block:
      - name: Restart replica
        module: core.service.restarted
        params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency falsely reported an external top-level probe as a peer dependency (%+v) - would break the valid restart case", info)
	}
}

// TestWithinBlock_RegisterSelfOK (guard #3) — a block child reads register.self.* (its
// own step's result, not a peer) in retry.until: → VALID. register.self is already
// filtered by ExtractRegisterRefs (collectTaskReads won't return it), so the detector
// must not react to it → ok==false.
func TestWithinBlock_RegisterSelfOK(t *testing.T) {
	const src = `
name: register_self
state_changes: {}
tasks:
  - name: Rolling group
    block:
      - name: Wait until healthy
        module: core.exec.run
        changed_when: false
        retry:
          count: 12
          delay: 5s
          until: "contains(register.self.stdout, 'master_link_status:up')"
        params: { cmd: redis-cli }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency falsely reported register.self as a peer dependency (%+v)", info)
	}
}

// TestWithinBlock_NestedPeerReject (guard #4) — a nested block: an inner child reads a
// register emitted by an OUTER child of the same (outer) block → reject. A peer inside
// a nested block = a peer outside (the whole block shares one Passage).
func TestWithinBlock_NestedPeerReject(t *testing.T) {
	const src = `
name: nested_peer
state_changes: {}
tasks:
  - name: Outer group
    block:
      - name: Probe role in outer
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Inner group
        block:
          - name: Restart on master deep
            module: core.service.restarted
            where: "register.role.stdout == 'master'"
            params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	info, bad := WithinBlockRegisterDependency(tasks)
	if !bad {
		t.Fatalf("WithinBlockRegisterDependency did NOT detect a peer-register through a nested block - silent-wrong-target")
	}
	if info.RegisterName != "role" {
		t.Errorf("RegisterName = %q, want role", info.RegisterName)
	}
}

// TestWithinBlock_NoRegisterOK (guard #5) — a block without a single register: inside
// → fast-path ok==false (nothing to read as a peer). Catches a regression where an
// empty blockEmits gives a false positive.
func TestWithinBlock_NoRegisterOK(t *testing.T) {
	const src = `
name: no_register
state_changes: {}
tasks:
  - name: Rolling group
    where: "soulprint.self.sid != ''"
    block:
      - name: Restart redis
        module: core.service.restarted
        params: { name: redis-server }
      - name: Reload config
        module: core.exec.run
        params: { cmd: reload }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency falsely triggered on a block without a register (%+v)", info)
	}
}

// TestWithinBlock_AcceptanceRestart (guard #6) — ★ ACCEPTANCE: the real
// examples/service/redis/scenario/restart/main.yml (probe redis_role top-level, block
// with where on an external register) is VALID → ok==false. Plus a regression loop
// over ALL committed example scenarios: none must be caught by the detector (else a
// valid example would silently stop running).
func TestWithinBlock_AcceptanceRestart(t *testing.T) {
	restartPath := filepath.FromSlash("../../examples/service/redis/scenario/restart/main.yml")
	m, _, diags, err := LoadScenarioManifest(restartPath, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest(restart): %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("restart diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	if info, bad := WithinBlockRegisterDependency(m.Tasks); bad {
		t.Fatalf("ACCEPTANCE BROKEN: restart/main.yml reported as within-block peer (%+v) - the external probe redis_role is valid", info)
	}

	// Acceptance regression: no committed example scenario is caught by the detector.
	all, gerr := filepath.Glob(filepath.FromSlash("../../examples/service/*/scenario/*/main.yml"))
	if gerr != nil {
		t.Fatalf("glob examples: %v", gerr)
	}
	if len(all) == 0 {
		t.Fatal("glob examples returned 0 scenarios - path/layout broken")
	}
	for _, p := range all {
		em, _, ediags, eerr := LoadScenarioManifest(p, ValidateOptions{})
		if eerr != nil || em == nil {
			continue // invalid/unresolvable offline example — not this detector's scope.
		}
		hardErr := false
		for _, d := range ediags {
			if d.Level == diag.LevelError {
				hardErr = true
				break
			}
		}
		if hardErr {
			continue
		}
		if info, bad := WithinBlockRegisterDependency(em.Tasks); bad {
			t.Errorf("acceptance regression: example %s caught by the within-block detector (%+v) - a valid example would stop running", p, info)
		}
	}
}
