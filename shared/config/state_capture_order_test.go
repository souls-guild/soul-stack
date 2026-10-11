package config

import "testing"

// Guard tests for the ordering rule of [ADR-0084] ("The ordering guard"). It is an
// offline ERROR in soul-lint, and it exists because the failure it describes is
// INVISIBLE in L0: the trial harness never crashes mid-run, so a store-after-use
// plan is green there and wrong in production.
//
// Every rule carries its ★ REVERSE cases: the orders that are legitimate must stay
// legal, or the guard stops being a guard and becomes a ban on the idiom.

// --- StoreAfterUse: generate → store → use ---

// TestStoreAfterUse_ConsumerBeforeCapture — ★ PRIMARY CASE. The generated password
// reaches a live host BEFORE the capture step commits it. Crash in that window and
// the host requires a password that exists nowhere: not in Postgres, not in Vault,
// not in the run's output. This is the exact failure ADR-0084 is written against.
func TestStoreAfterUse_ConsumerBeforeCapture(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Configure the host with the generated password
    module: core.exec.run
    changed_when: false
    params:
      cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`
	info, bad := StoreAfterUse(loadTasks(t, src))
	if !bad {
		t.Fatal("StoreAfterUse = false, want true (the host is configured before the value is stored)")
	}
	if info.Ref != "gen" {
		t.Errorf("Ref = %q, want %q", info.Ref, "gen")
	}
	if info.CaptureName != "Capture the admin password" {
		t.Errorf("CaptureName = %q", info.CaptureName)
	}
	if info.CaptureVerb != "set" {
		t.Errorf("CaptureVerb = %q, want %q", info.CaptureVerb, "set")
	}
	if info.OtherName != "Configure the host with the generated password" {
		t.Errorf("OtherName = %q", info.OtherName)
	}
}

// TestStoreAfterUse_PlanOrderNotPassage — ★ GUARD (ADR-0091, S1: the rule
// is re-keyed to plan order). The capture waits on a readiness probe, so the
// derivation would put it in Passage 2 and the replica's consumer, written after
// it, in Passage 1 — "use before store". A run executes steps in the order
// written, so the capture stores the value first and the plan is crash-safe.
//
// Mutation: order the pair by Stratify's Passages again → this plan is refused.
func TestStoreAfterUse_PlanOrderNotPassage(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Probe the service
    module: core.exec.run
    register: probe
    changed_when: false
    params:
      cmd: "redis-cli ping"
  - name: Wait for the service to settle
    module: core.exec.run
    register: ready
    changed_when: false
    params:
      cmd: "test ${ register.probe.rc } -eq 0"
  - name: Capture the admin password once the service is ready
    module: core.state.set
    vars:
      settled: "${ register.ready.rc }"
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
  - name: Configure the replica with the generated password
    module: core.exec.run
    on: ["replica"]
    changed_when: false
    params:
      cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
`
	tasks := loadTasks(t, src)
	derived, err := Stratify(tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if got := derived.TaskPassage; got[3] <= got[4] {
		t.Fatalf("TaskPassage = %v, want the capture (#3) derived STRICTLY after the consumer (#4) -- the fixture no longer tells plan order from Passage order", got)
	}
	if info, bad := StoreAfterUse(tasks); bad {
		t.Fatalf("StoreAfterUse = true (%q before %q), want false -- the capture is written first and runs first", info.OtherName, info.CaptureName)
	}
}

// TestStoreAfterUse_ConsumerWrittenFirstRunsFirst — the mirror of
// [TestStoreAfterUse_PlanOrderNotPassage]. The consumer waits on a readiness probe,
// so the derivation would push it a Passage past the capture and call the order
// safe; a run executes it first, and the host is configured with a value not yet
// stored.
func TestStoreAfterUse_ConsumerWrittenFirstRunsFirst(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Probe the service
    module: core.exec.run
    register: probe
    changed_when: false
    params:
      cmd: "redis-cli ping"
  - name: Wait for the service to settle
    module: core.exec.run
    register: ready
    changed_when: false
    params:
      cmd: "test ${ register.probe.rc } -eq 0"
  - name: Configure the host once the service is ready
    module: core.exec.run
    changed_when: false
    vars:
      settled: "${ register.ready.rc }"
    params:
      cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`
	tasks := loadTasks(t, src)
	derived, err := Stratify(tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if got := derived.TaskPassage; got[3] <= got[4] {
		t.Fatalf("TaskPassage = %v, want the consumer (#3) derived STRICTLY after the capture (#4) -- the fixture no longer tells plan order from Passage order", got)
	}
	info, bad := StoreAfterUse(tasks)
	if !bad {
		t.Fatal("StoreAfterUse = false, want true (the consumer runs before the capture stores the value)")
	}
	if info.OtherName != "Configure the host once the service is ready" || info.Ref != "gen" {
		t.Errorf("info = %+v", info)
	}
}

// TestStoreAfterUse_ConsumerNestedInABlock — the consumer is a block CHILD, and the
// block itself reads nothing. The capture cannot join it (a keeper task in a block is
// refused, block_on_keeper_invalid), so the only way the defect is visible at all is
// the walk descending into the block's children: judged on the top-level tasks alone,
// the reader of `register.gen` does not exist and the plan looks crash-safe.
func TestStoreAfterUse_ConsumerNestedInABlock(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Configure the hosts
    block:
      - name: Configure the host with the generated password
        module: core.exec.run
        changed_when: false
        params:
          cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`
	info, bad := StoreAfterUse(loadTasks(t, src))
	if !bad {
		t.Fatal("StoreAfterUse = false, want true (the block child uses the value before the capture stores it)")
	}
	if info.OtherName != "Configure the host with the generated password" {
		t.Errorf("OtherName = %q, want the block CHILD -- the parent block reads nothing", info.OtherName)
	}
}

// TestStoreAfterUse_Reverse — ★ REVERSE cases. Each is an order the guard must leave
// alone; a rule that rejects any of them makes the crash-safe idiom itself unwritable.
func TestStoreAfterUse_Reverse(t *testing.T) {
	cases := []struct {
		name string
		why  string
		src  string
	}{
		{
			name: "capture-then-consumer",
			why:  "generate -> store -> use is the prescribed order",
			src: `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
  - name: Configure the host with the stored password
    module: core.exec.run
    changed_when: false
    params:
      cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
`,
		},
		{
			name: "two-captures-same-register",
			why:  "two captures of one register are two stores; their relative order carries no crash-safety meaning",
			src: `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
  - name: Mirror it into the rotation field
    module: core.state.set
    params:
      field: admin_password_previous
      value: "${ register.gen.stdout }"
`,
		},
		{
			name: "consumer-in-a-block-after-the-capture",
			why:  "generate -> store -> use is the correct order, and a block around the use does not change it",
			src: `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
  - name: Configure the hosts
    block:
      - name: Configure the host with the generated password
        module: core.exec.run
        changed_when: false
        params:
          cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
`,
		},
		{
			name: "consumer-of-another-register",
			why:  "the consumer must read the SAME register the capture stores",
			src: `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Probe the listening port
    module: core.exec.run
    register: port
    changed_when: false
    params:
      cmd: "ss -ltnp"
  - name: Report the port
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ register.port.stdout }"
  - name: Capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`,
		},
		{
			name: "no-capture-at-all",
			why:  "a plan without a core.state.<verb> step has nothing to order against",
			src: `
name: create
tasks:
  - name: Generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
  - name: Configure the host with the generated password
    module: core.exec.run
    changed_when: false
    params:
      cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if info, bad := StoreAfterUse(loadTasks(t, tc.src)); bad {
				t.Fatalf("StoreAfterUse = true (%q before %q on %q), want false -- %s",
					info.OtherName, info.CaptureName, info.Ref, tc.why)
			}
		})
	}
}
