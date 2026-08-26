package config

import "testing"

// Guard tests for the two ordering rules of [ADR-0084] ("The ordering guard").
// Both are offline ERRORs in soul-lint, and both exist because the failure they
// describe is INVISIBLE in L0: the trial harness threads state task-by-task and
// never crashes mid-run, so a store-after-use plan and a stale same-Passage read
// are both green there and wrong in production.
//
// Every rule carries its ★ REVERSE cases: the orders that are legitimate must stay
// legal, or the guard stops being a guard and becomes a ban on the idiom.

func orderPlan(t *testing.T, src string) ([]Task, Passage) {
	t.Helper()
	tasks := loadTasks(t, src)
	p, err := Stratify(tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	return tasks, p
}

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
    on: keeper
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`
	tasks, p := orderPlan(t, src)
	info, bad := StoreAfterUse(tasks, p)
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

// TestStoreAfterUse_ConsumerInEarlierPassage — the same defect with the plan order
// REVERSED against the execution order: the capture waits on a readiness probe and
// lands in Passage 2, while the consumer that already used the password sits in
// Passage 1 despite a HIGHER plan index. Reading the raw task order here answers
// "capture first" and misses the defect entirely — this is why the rule compares
// Stratify's Passages before it compares indices.
func TestStoreAfterUse_ConsumerInEarlierPassage(t *testing.T) {
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
    on: keeper
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
	tasks, p := orderPlan(t, src)
	if got := p.TaskPassage; got[3] <= got[4] {
		t.Fatalf("TaskPassage = %v, want the capture (#3) STRICTLY after the consumer (#4) -- the fixture no longer tests Passage-over-index ordering", got)
	}
	info, bad := StoreAfterUse(tasks, p)
	if !bad {
		t.Fatal("StoreAfterUse = false, want true (the consumer runs a whole Passage before the capture)")
	}
	if info.Ref != "gen" {
		t.Errorf("Ref = %q, want %q", info.Ref, "gen")
	}
	if info.OtherName != "Configure the replica with the generated password" {
		t.Errorf("OtherName = %q", info.OtherName)
	}
	if info.OtherPassage >= info.CapturePassage {
		t.Errorf("OtherPassage = %d, CapturePassage = %d, want the consumer STRICTLY earlier",
			info.OtherPassage, info.CapturePassage)
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
    on: keeper
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`
	tasks, p := orderPlan(t, src)
	info, bad := StoreAfterUse(tasks, p)
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
    on: keeper
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
    on: keeper
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
  - name: Mirror it into the rotation field
    module: core.state.set
    on: keeper
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
    on: keeper
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
    on: keeper
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
			tasks, p := orderPlan(t, tc.src)
			if info, bad := StoreAfterUse(tasks, p); bad {
				t.Fatalf("StoreAfterUse = true (%q before %q on %q), want false -- %s",
					info.OtherName, info.CaptureName, info.Ref, tc.why)
			}
		})
	}
}

// --- StaleStateRead: an interpolated state read after a same-Passage capture ---

// TestStaleStateRead_ReaderAfterCapture — ★ PRIMARY CASE. `${ incarnation.state.x }`
// refreshes only at a Passage boundary, so a reader in the capture's own Passage
// renders the PRE-capture value while the verb engine, which always reads live, sees
// the new one. Two mechanisms, one field, two answers.
func TestStaleStateRead_ReaderAfterCapture(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Announce the endpoint
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state.endpoint }"
`
	tasks, p := orderPlan(t, src)
	info, bad := StaleStateRead(tasks, p)
	if !bad {
		t.Fatal("StaleStateRead = false, want true (same-Passage read of a just-captured field)")
	}
	if info.Ref != "endpoint" {
		t.Errorf("Ref = %q, want %q", info.Ref, "endpoint")
	}
	if info.CaptureName != "Capture the endpoint" || info.OtherName != "Announce the endpoint" {
		t.Errorf("names = (%q, %q)", info.CaptureName, info.OtherName)
	}
	if info.CapturePassage != info.OtherPassage {
		t.Errorf("passages = (%d, %d), want equal -- the rule is same-Passage only",
			info.CapturePassage, info.OtherPassage)
	}
}

// TestStaleStateRead_IndexForm — the same read written as a string index. CEL accepts
// both spellings for the same field; a rule that only knows the dotted one is a rule
// an author steps around by accident.
func TestStaleStateRead_IndexForm(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Announce the endpoint
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state['endpoint'] }"
`
	tasks, p := orderPlan(t, src)
	info, bad := StaleStateRead(tasks, p)
	if !bad {
		t.Fatal("StaleStateRead = false, want true (incarnation.state['endpoint'] is the same read)")
	}
	if info.Ref != "endpoint" {
		t.Errorf("Ref = %q, want %q", info.Ref, "endpoint")
	}
}

// TestStaleStateRead_Reverse — ★ REVERSE cases: every read that renders the value the
// author actually meant.
func TestStaleStateRead_Reverse(t *testing.T) {
	cases := []struct {
		name string
		why  string
		src  string
	}{
		{
			name: "reader-before-capture",
			why:  "reading the pre-capture value BEFORE the capture is what the author asked for",
			src: `
name: create
tasks:
  - name: Announce the previous endpoint
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state.endpoint }"
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    params:
      field: endpoint
      value: "10.0.0.1"
`,
		},
		{
			name: "reader-in-a-later-passage",
			why:  "the runner re-reads incarnation.state at a Passage boundary, so a later Passage renders the captured value",
			src: `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    register: captured
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Announce the endpoint
    module: core.exec.run
    changed_when: false
    vars:
      committed: "${ register.captured.changed }"
    params:
      cmd: "echo ${ incarnation.state.endpoint } ${ committed }"
`,
		},
		{
			name: "reader-of-another-field",
			why:  "an untouched field renders the same value before and after the capture",
			src: `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Announce the port
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state.port }"
`,
		},
		{
			name: "capture-field-is-an-expression",
			why:  "which field the capture writes is decided at render -- offline the rule must not guess",
			src: `
name: create
tasks:
  - name: Capture a computed field
    module: core.state.set
    on: keeper
    vars:
      target: endpoint
    params:
      field: "${ target }"
      value: "10.0.0.1"
  - name: Announce the endpoint
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state.endpoint }"
`,
		},
		{
			name: "no-capture-at-all",
			why:  "an interpolated state read without any capture is the ordinary pre-run snapshot",
			src: `
name: create
tasks:
  - name: Announce the endpoint
    module: core.exec.run
    changed_when: false
    params:
      cmd: "echo ${ incarnation.state.endpoint }"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks, p := orderPlan(t, tc.src)
			if info, bad := StaleStateRead(tasks, p); bad {
				t.Fatalf("StaleStateRead = true (%q reads %q after %q), want false -- %s",
					info.OtherName, info.Ref, info.CaptureName, tc.why)
			}
		})
	}
}

// TestStaleStateRead_ReadInWhere — the read zone matters, not just the order:
// `where:` is rendered Keeper-side from the same snapshot as params, so a stale field
// there decides WHICH HOSTS the task runs on. Same defect, worse blast radius.
func TestStaleStateRead_ReadInWhere(t *testing.T) {
	const src = `
name: create
tasks:
  - name: Capture the endpoint
    module: core.state.set
    on: keeper
    params:
      field: endpoint
      value: "10.0.0.1"
  - name: Configure every host that is not the endpoint
    module: core.exec.run
    where: "soulprint.self.network.primary_ip != incarnation.state.endpoint"
    changed_when: false
    params:
      cmd: "redis-cli replicaof 10.0.0.1 6379"
`
	tasks, p := orderPlan(t, src)
	info, bad := StaleStateRead(tasks, p)
	if !bad {
		t.Fatal("StaleStateRead = false, want true (a stale where: picks the wrong hosts)")
	}
	if info.Ref != "endpoint" {
		t.Errorf("Ref = %q, want %q", info.Ref, "endpoint")
	}
}
