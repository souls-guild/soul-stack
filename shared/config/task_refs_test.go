package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// countCode returns how many diagnostics in the set carry the given code.
func countCode(diags []diag.Diagnostic, code string) int {
	n := 0
	for i := range diags {
		if diags[i].Code == code {
			n++
		}
	}
	return n
}

// --- destiny tasks (flat top-level list) ---

func TestTaskRefs_DuplicateRegister_Destiny(t *testing.T) {
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: rewrite conf (copy-paste, register not renamed)
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis2.conf
    content: y
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !diag.HasErrors(diags) {
		t.Fatal("expected a duplicate_task_address error")
	}
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Fatalf("duplicate_task_address count = %d, want 1 (on the second declaration); diags=%v", got, diags)
	}
	// Diagnostic points at the SECOND declaration (the first is primary).
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].register" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].register", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownOnChanges_Destiny(t *testing.T) {
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart on typo'd ref
  module: core.service.restarted
  onchanges: [redis_cnf]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference count = %d, want 1; diags=%v", got, diags)
	}
	for _, d := range diags {
		if d.Code == "unknown_register_reference" && d.YAMLPath != "$[1].onchanges[0]" {
			t.Errorf("YAMLPath = %q, want $[1].onchanges[0]", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownOnFail_Destiny(t *testing.T) {
	src := `
- name: migrate
  module: core.exec.run
  register: migrate_db
  params:
    cmd: migrate
- name: rescue
  module: core.exec.run
  onfail: [migrate_dbb]
  params:
    cmd: rollback
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference (onfail) count = %d, want 1; diags=%v", got, diags)
	}
}

func TestTaskRefs_UnknownRequire_Destiny(t *testing.T) {
	src := `
- name: prepare
  module: core.exec.run
  register: prep
  params:
    cmd: prepare
- name: act
  module: core.exec.run
  require: [prepp]
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference (require) count = %d, want 1; diags=%v", got, diags)
	}
}

func TestTaskRefs_RequireAll_NotFlagged(t *testing.T) {
	// require: "all" is a scalar, not a register list; must not be caught by cross-ref.
	src := `
- name: prepare
  module: core.exec.run
  register: prep
  params:
    cmd: prepare
- name: act
  module: core.exec.run
  require: all
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("require: all must not produce unknown_register_reference; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_Valid_Destiny(t *testing.T) {
	// Unique register + existing references + CEL-wrapped onchanges → OK.
	src := `
- name: write conf
  module: core.file.rendered
  register: redis_conf
  params:
    path: /etc/redis.conf
    template: redis.conf.tmpl
- name: harden
  module: core.file.present
  register: redis_hardening
  params:
    path: /etc/redis-hardening
    content: x
- name: restart
  module: core.service.restarted
  onchanges: [redis_conf, redis_hardening]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address") + countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("a valid plan must not produce cross-ref errors; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_CELRef_NotFlagged(t *testing.T) {
	// CEL-wrapped element in onchanges (dynamic resolve) is skipped statically,
	// not flagged as unknown.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart
  module: core.service.restarted
  onchanges: ["${ vars.dynamic_ref }"]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("a CEL wrapper in onchanges must not be flagged; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_BlockNested_Destiny(t *testing.T) {
	// A register inside a block is visible from top-level onchanges (flat plan
	// namespace); a duplicate register between block and top-level is an error.
	src := `
- name: group
  block:
    - name: inner write
      module: core.file.present
      register: inner_conf
      params:
        path: /etc/inner
        content: x
- name: restart references inner block register
  module: core.service.restarted
  onchanges: [inner_conf]
  params:
    name: redis
- name: duplicate of inner block register at top-level
  module: core.file.present
  register: inner_conf
  params:
    path: /etc/other
    content: y
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	// onchanges:[inner_conf] resolves (block register visible) → no unknown ref.
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Errorf("block register must be visible from top-level onchanges; got unknown=%d, diags=%v", got, diags)
	}
	// top-level register inner_conf duplicates the block register → error.
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Errorf("a duplicate register between block and top-level must be flagged; got=%d, diags=%v", got, diags)
	}
}

// --- T2: uniqueness of the register ∪ id address space (per-file) ---

func TestTaskAddress_DuplicateID_Destiny(t *testing.T) {
	// Two ids with the same value → duplicate subscription address.
	src := `
- name: reload sysctl
  id: tuned
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: reload again
  id: tuned
  module: core.sysctl.present
  params:
    name: vm.dirty_ratio
    value: "20"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("a duplicate id must be flagged; duplicate_task_address count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].id" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].id", d.YAMLPath)
		}
	}
}

func TestTaskAddress_IDCollidesRegister_Destiny(t *testing.T) {
	// One task's id == another's register → collision in the shared space.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: reload sysctl
  id: redis_conf
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("a register/id collision must be flagged; count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].id" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].id", d.YAMLPath)
		}
	}
}

func TestTaskAddress_RegisterCollidesID_Destiny(t *testing.T) {
	// Reverse order: id before register with the same name. Diagnostic points at
	// the second declaration (register).
	src := `
- name: reload sysctl
  id: shared_name
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: write conf
  module: core.file.present
  register: shared_name
  params:
    path: /etc/redis.conf
    content: x
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("an id/register collision must be flagged; count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].register" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].register", d.YAMLPath)
		}
	}
}

func TestTaskAddress_UniqueIDAndRegister_Destiny(t *testing.T) {
	// Unique register + id → OK.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: reload sysctl
  id: sysctl_reloaded
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 0 {
		dump(t, diags)
		t.Fatalf("unique register+id must not produce a duplicate; count = %d, want 0", got)
	}
}

func TestTaskAddress_IDNotResolvableInRequisites_Destiny(t *testing.T) {
	// id addresses a subscription but does NOT create register.<name>: an onchanges
	// reference to an id-task is unknown_register_reference (id is not equal to
	// register in cross-ref, though it shares the uniqueness space).
	src := `
- name: reload sysctl
  id: sysctl_reloaded
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: restart referencing an id (not a register)
  module: core.service.restarted
  onchanges: [sysctl_reloaded]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		dump(t, diags)
		t.Fatalf("an onchanges reference to an id-task must be unknown_register_reference; count = %d, want 1", got)
	}
	// No address duplicate here (id and register reference are different names).
	if got := countCode(diags, "duplicate_task_address"); got != 0 {
		t.Errorf("unexpected duplicate_task_address; count = %d, want 0", got)
	}
}

// --- scenario (main.yml with wrapper) ---

func TestTaskRefs_DuplicateRegister_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: a
    module: core.exec.run
    register: probe
    params:
      cmd: echo a
  - name: b
    module: core.exec.run
    register: probe
    params:
      cmd: echo b
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Fatalf("duplicate_task_address count = %d, want 1; diags=%v", got, diags)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$.tasks[1].register" {
			t.Errorf("YAMLPath = %q, want $.tasks[1].register", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownReference_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: probe
    module: core.exec.run
    register: redis_role
    params:
      cmd: redis-cli role
  - name: restart
    module: core.service.restarted
    onchanges: [redis_rolee]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference count = %d, want 1; diags=%v", got, diags)
	}
}

// --- O6: type validation of onchanges/onfail/require ---

func TestTaskTypes_OnChangesScalar_Rejected(t *testing.T) {
	// Scalar instead of a list passed silently before O6 (cross-ref only looks at
	// sequences). Now type_mismatch.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart
  module: core.service.restarted
  onchanges: redis_conf
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].onchanges") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on $[1].onchanges (scalar instead of a list)")
	}
}

func TestTaskTypes_OnFailScalar_Rejected(t *testing.T) {
	src := `
- name: migrate
  module: core.exec.run
  register: migrate_db
  params:
    cmd: migrate
- name: rescue
  module: core.exec.run
  onfail: migrate_db
  params:
    cmd: rollback
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].onfail") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on $[1].onfail (scalar instead of a list)")
	}
}

func TestTaskTypes_RequireAll_Accepted(t *testing.T) {
	// require: all is the only legitimate scalar form.
	src := `
- name: act
  module: core.exec.run
  require: all
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "type_mismatch") != 0 {
		dump(t, diags)
		t.Fatalf("require: all must not produce type_mismatch")
	}
}

func TestTaskTypes_RequireScalarOther_Rejected(t *testing.T) {
	// require: <non-all scalar> is an error (must be a list or "all").
	src := `
- name: prep
  module: core.exec.run
  register: prep
  params:
    cmd: prep
- name: act
  module: core.exec.run
  require: prep
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].require") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on $[1].require (scalar != all)")
	}
}

func TestTaskTypes_RequireList_Accepted(t *testing.T) {
	src := `
- name: prep
  module: core.exec.run
  register: prep
  params:
    cmd: prep
- name: act
  module: core.exec.run
  require: [prep]
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "type_mismatch") != 0 {
		dump(t, diags)
		t.Fatalf("require: [prep] must not produce type_mismatch")
	}
}

func TestTaskTypes_OnChangesNonStringElem_Rejected(t *testing.T) {
	// An int element in the onchanges list → type_mismatch on the element.
	src := `
- name: restart
  module: core.service.restarted
  onchanges: [42]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[0].onchanges[0]") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on $[0].onchanges[0] (int element)")
	}
}

// --- O3: CEL cross-ref register in predicates ---

func TestTaskRefs_CELWhenUnknown_Destiny(t *testing.T) {
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: redis-cli role
- name: restart
  module: core.service.restarted
  when: register.redis_rolee.changed
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_register_reference", "$[1].when") {
		dump(t, diags)
		t.Fatalf("expected unknown_register_reference on $[1].when (typo in a register name in CEL)")
	}
}

func TestTaskRefs_CELWhenKnown_Destiny(t *testing.T) {
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: redis-cli role
- name: restart
  module: core.service.restarted
  when: register.redis_role.changed && register.redis_role.stdout == 'ok'
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("an existing register name in when must not be flagged")
	}
}

func TestTaskRefs_CELSelf_NotFlagged(t *testing.T) {
	// register.self is the current task, not flagged (forward-ref to itself).
	src := `
- name: probe
  module: core.cmd.shell
  changed_when: false
  failed_when: register.self.stdout != 'up'
  retry:
    count: 3
    delay: 5s
    until: register.self.stdout == 'up'
  params:
    cmd: redis-cli ping
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("register.self must not be flagged by the cross-ref check")
	}
}

func TestTaskRefs_CELStringLiteral_NotFalsePositive(t *testing.T) {
	// register.foo inside a CEL string literal is data, not an identifier;
	// must not produce a false unknown_register_reference.
	src := `
- name: log
  module: core.exec.run
  failed_when: "register.self.stdout == 'register.ghost not found'"
  params:
    cmd: check
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("register.<x> inside a string literal must not be flagged")
	}
}

func TestTaskRefs_CELRetryUntilLoopWhere_Unknown(t *testing.T) {
	// retry.until, loop.when, where — all predicate positions are covered.
	src := `
- name: act
  module: core.exec.run
  where: register.missing_a.changed
  loop:
    items: ${ input.xs }
    as: x
    when: register.missing_b.ok
  retry:
    count: 2
    delay: 1s
    until: register.missing_c.done
  params:
    cmd: act ${ x }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 3 {
		dump(t, diags)
		t.Fatalf("expected 3 unknown_register_reference (where/loop.when/retry.until); got=%d", got)
	}
}

func TestTaskRefs_CELDynamicAccess_NotFlagged(t *testing.T) {
	// register["..."] is dynamic access, not matched by the form, not flagged.
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: probe
- name: act
  module: core.exec.run
  when: register["redis_role"].changed
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("a dynamic register[...] must not produce unknown_register_reference")
	}
}

func TestTaskRefs_Valid_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: probe
    module: core.exec.run
    register: redis_role
    params:
      cmd: redis-cli role
  - name: restart
    module: core.service.restarted
    onchanges: [redis_role]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address") + countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("a valid scenario must not produce cross-ref errors; got=%d, diags=%v", got, diags)
	}
}

// TestTaskRefs_UnknownInterpField_Destiny covers the closed ADR-056 S2 gap: the
// cross-ref validator did not previously walk interpolation source fields
// (vars/output/params/loop.items), so an unknown register in them survived to the
// runtime stratifier. Now each field is caught offline. The field set here must
// match the passage-defining sources of the stratifier (reads==refs, render/passage_test.go).
func TestTaskRefs_UnknownInterpField_Destiny(t *testing.T) {
	cases := map[string]string{
		"output": `
- name: act
  module: core.exec.run
  changed_when: false
  output:
    role: "${ register.ghost.stdout }"
  params: { cmd: "true" }
`,
		"vars": `
- name: act
  module: core.exec.run
  changed_when: false
  vars:
    v: "${ register.ghost.stdout }"
  params: { cmd: "true" }
`,
		"params": `
- name: act
  module: core.exec.run
  changed_when: false
  params:
    cmd: echo
    args: ["${ register.ghost.stdout }"]
`,
		"loop.items": `
- name: act
  module: core.exec.run
  changed_when: false
  loop:
    items: "${ register.ghost.stdout }"
    as: item
  params: { cmd: "echo ${ item }" }
`,
	}
	for field, src := range cases {
		t.Run(field, func(t *testing.T) {
			_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
			if got := countCode(diags, "unknown_register_reference"); got != 1 {
				dump(t, diags)
				t.Fatalf("%s: unknown_register_reference count = %d, want 1 (ADR-056 S2 validator gap not closed)", field, got)
			}
		})
	}
}

// TestTaskRefs_UnknownApplyInput_Scenario — applier task: a register that nobody
// emits, used in apply.input → unknown_register_reference (apply lives only in
// scenario).
func TestTaskRefs_UnknownApplyInput_Scenario(t *testing.T) {
	src := `
name: act
tasks:
  - name: delegate
    apply:
      destiny: redis
      input:
        seed: "${ register.ghost.stdout }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/act/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		dump(t, diags)
		t.Fatalf("apply.input: unknown_register_reference count = %d, want 1", got)
	}
}

// TestTaskRefs_KnownInterpField_NotFlagged — a register emitted by a probe task,
// read via ${ … } in output/params/apply.input, is valid and does NOT produce a
// false unknown_register_reference (forward-ref across the flat plan namespace).
func TestTaskRefs_KnownInterpField_NotFlagged(t *testing.T) {
	src := `
name: chain
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    changed_when: false
    params: { cmd: "true" }
  - name: use in output and params
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.probe.stdout }"
    params:
      cmd: echo
      args: ["${ register.probe.stdout }"]
  - name: use in apply input
    apply:
      destiny: redis
      input:
        seed: "${ register.probe.stdout }"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/chain/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		dump(t, diags)
		t.Fatalf("a known register in output/params/apply.input must not be flagged; got=%d", got)
	}
}

// TestTaskRefs_OnChangesApplierRegister_Valid — ★ guard (e), applier-register
// materialization (orchestration.md §2.1.1, Variant B). A register: ON an applier
// task (`apply:`+`register:`) is a VALID subscription address: collectAddresses
// registers register: of any task (apply/module/block alike at the AST level), so
// an external onchanges:[<applier-register>] must NOT be flagged as
// unknown_register_reference. Invariant: an applier-register is addressable from
// outside (the terminal core.noop.run materializes it, register.<applier> resolves).
func TestTaskRefs_OnChangesApplierRegister_Valid(t *testing.T) {
	src := `
name: act
tasks:
  - name: apply redis destiny
    register: redis_destiny
    apply:
      destiny: redis
      input:
        action: update_acls
  - name: notify on destiny change
    module: core.service.restarted
    onchanges: [redis_destiny]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/act/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		dump(t, diags)
		t.Fatalf("onchanges on an applier-register must not be flagged unknown_register_reference; got=%d (applier-register is a valid subscription address)", got)
	}
}

// TestTaskRefs_OnChangesApplierRegister_Typo — ★ guard (e) reverse side: a TYPO in
// onchanges on an applier-register is still caught as unknown_register_reference
// (collectAddresses knows only the real name redis_destiny, not redis_destinyy).
func TestTaskRefs_OnChangesApplierRegister_Typo(t *testing.T) {
	src := `
name: act
tasks:
  - name: apply redis destiny
    register: redis_destiny
    apply:
      destiny: redis
      input:
        action: update_acls
  - name: notify on typo'd ref
    module: core.service.restarted
    onchanges: [redis_destinyy]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/act/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		dump(t, diags)
		t.Fatalf("a typo in onchanges on an applier-register must be flagged; unknown_register_reference count = %d, want 1", got)
	}
}
