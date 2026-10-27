package config

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Step numbering for linear dispatch (ADR-0091, §`passage`, fork 3): a
// default block's children are steps of their own, a block rolled as one group is
// one, and the shape comes from the file alone.

func loadLinearManifest(t *testing.T, src string) *ScenarioManifest {
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
	return m
}

const linearOverlayAndGroup = `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: overlay
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        where: register.a.stdout == 'yes'
        params: { cmd: b }
  - name: canary
    serial: 1
    settings:
      serial_scope: block
    block:
      - name: change
        module: core.exec.run
        params: { cmd: change }
      - name: check
        module: core.exec.run
        params: { cmd: check }
  - name: tail
    module: core.exec.run
    params: { cmd: tail }
`

// The acceptance shape of the ticket: a default block [a, b] is steps 1 and 2, a
// group of two is the one step 3, and the next task keeps counting after both.
func TestLinearPassage_DefaultBlockChildrenAreStepsGroupIsOne(t *testing.T) {
	m := loadLinearManifest(t, linearOverlayAndGroup)
	plan, steps := LinearPassage(m.Tasks, m.Settings)

	if want := []int{0, 1, 3, 4}; !reflect.DeepEqual(plan.TaskPassage, want) {
		t.Errorf("TaskPassage = %v, want %v", plan.TaskPassage, want)
	}
	if plan.Count != 5 {
		t.Errorf("Count = %d, want 5", plan.Count)
	}
	if got := steps[1].Children; len(got) != 2 || got[0].Step != 1 || got[1].Step != 2 {
		t.Errorf("default block children = %+v, want steps 1 and 2", got)
	}
	if steps[1].Group {
		t.Error("default block marked as a group")
	}
	if !steps[2].Group || steps[2].Children != nil {
		t.Errorf("canary node = %+v, want one group step without children", steps[2])
	}
}

// ★ GUARD (NIM-998, settings.md §3.1). A block with serial: and no serial_scope
// written anywhere rolls by task: each child is a step of its own at the block's
// width, so upgrade reaches every host before check starts. Only a written
// serial_scope: block makes it the canary, one group step.
//
// Mutation: default serial_scope to block in DefaultScenarioSettings → the
// unwritten block is one group step.
func TestLinearPassage_SerialBlockWithoutScopeRollsByTask(t *testing.T) {
	const tmpl = `
name: linear
tasks:
  - name: roll
    serial: 1
%s    block:
      - name: upgrade
        module: core.exec.run
        params: { cmd: upgrade }
      - name: check
        module: core.exec.run
        params: { cmd: check }
`
	m := loadLinearManifest(t, fmt.Sprintf(tmpl, ""))
	plan, steps := LinearPassage(m.Tasks, m.Settings)
	if plan.Count != 2 || steps[0].Group || len(steps[0].Children) != 2 {
		t.Fatalf("Count = %d, node = %+v, want a default block of two steps", plan.Count, steps[0])
	}
	for _, c := range m.Tasks[0].Block.Block {
		if got := LinearBlockChild(m.Tasks[0], c).Serial; fmt.Sprint(got) != "1" {
			t.Errorf("child %q serial = %v, want the block's 1", c.Name, got)
		}
	}

	m = loadLinearManifest(t, fmt.Sprintf(tmpl, "    settings: { serial_scope: block }\n"))
	plan, steps = LinearPassage(m.Tasks, m.Settings)
	if plan.Count != 1 || !steps[0].Group {
		t.Errorf("Count = %d, node = %+v, want one group step", plan.Count, steps[0])
	}
}

// serial_scope written at the scenario level makes every block that has a serial:
// a group, and only those: on a block without one it is unused, not a mode.
func TestLinearPassage_ScenarioLevelSerialScope(t *testing.T) {
	m := loadLinearManifest(t, `
name: linear
settings:
  serial_scope: block
tasks:
  - name: rolled
    serial: 2
    block:
      - name: a
        module: core.exec.run
        params: { cmd: a }
      - name: b
        module: core.exec.run
        params: { cmd: b }
  - name: plain
    block:
      - name: c
        module: core.exec.run
        params: { cmd: c }
      - name: d
        module: core.exec.run
        params: { cmd: d }
`)
	plan, steps := LinearPassage(m.Tasks, m.Settings)
	if want := []int{0, 1}; !reflect.DeepEqual(plan.TaskPassage, want) || plan.Count != 3 {
		t.Fatalf("TaskPassage = %v Count = %d, want [0 1] and 3", plan.TaskPassage, plan.Count)
	}
	if !steps[0].Group {
		t.Error("block with serial: under scenario serial_scope: block is not a group")
	}
	if steps[1].Group || len(steps[1].Children) != 2 {
		t.Errorf("block without serial: = %+v, want a default block of two steps", steps[1])
	}
}

// A nested block inherits serial: from a default block above it, so with
// serial_scope: block it is a group although it writes no serial: of its own.
func TestLinearPassage_NestedBlockInheritsSerial(t *testing.T) {
	m := loadLinearManifest(t, `
name: linear
tasks:
  - name: outer
    serial: 1
    settings:
      serial_scope: task
    block:
      - name: first
        module: core.exec.run
        params: { cmd: first }
      - name: inner
        settings:
          serial_scope: block
        block:
          - name: x
            module: core.exec.run
            params: { cmd: x }
          - name: y
            module: core.exec.run
            params: { cmd: y }
`)
	plan, steps := LinearPassage(m.Tasks, m.Settings)
	if plan.Count != 2 {
		t.Fatalf("Count = %d, want 2 (first, then the inner group)", plan.Count)
	}
	inner := steps[0].Children[1]
	if !inner.Group || inner.Step != 1 {
		t.Errorf("inner block = %+v, want a group at step 1", inner)
	}
}

// Every top-level entry that is not a default block is one step, a keeper-side
// task and an assert included, and an empty block takes one as well, so no index
// lands outside Count.
func TestLinearPassage_EveryOtherTaskIsOneStep(t *testing.T) {
	tasks := []Task{
		{Name: "check", Assert: &AssertSpec{That: []string{"true"}}},
		{Name: "provision", Module: &ModuleTask{Module: "core.state.set"}},
		{Name: "empty", Block: &BlockTask{}},
		{Name: "loop", Module: &ModuleTask{Module: "core.exec.run"}, Loop: &LoopSpec{Items: []any{1, 2}}},
	}
	plan, _ := LinearPassage(tasks, nil)
	if want := []int{0, 1, 2, 3}; !reflect.DeepEqual(plan.TaskPassage, want) || plan.Count != 4 {
		t.Errorf("TaskPassage = %v Count = %d, want %v and 4", plan.TaskPassage, plan.Count, want)
	}
	if plan, _ := LinearPassage(nil, nil); plan.Count != 1 {
		t.Errorf("empty plan Count = %d, want 1", plan.Count)
	}
}

// within_block_register_dependency narrows to groups under linear dispatch (ADR
// item 12): a default block's later child reading an earlier child's register is
// an ordinary later step, the same read inside a group is still refused, and the
// staged check is untouched.
func TestWithinGroupRegisterDependency_OnlyGroupsAreRefused(t *testing.T) {
	m := loadLinearManifest(t, linearOverlayAndGroup)
	if info, bad := WithinGroupRegisterDependency(m.Tasks, m.Settings); bad {
		t.Errorf("default block refused under linear dispatch: %+v", info)
	}
	if _, bad := WithinBlockRegisterDependency(m.Tasks); !bad {
		t.Error("staged check no longer refuses the default block's peer read")
	}

	grouped := loadLinearManifest(t, `
name: linear
tasks:
  - name: canary
    serial: 1
    settings:
      serial_scope: block
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: b
        module: core.exec.run
        params: { cmd: "${ register.a.stdout }" }
`)
	info, bad := WithinGroupRegisterDependency(grouped.Tasks, grouped.Settings)
	if !bad {
		t.Fatal("group's peer read not refused under linear dispatch")
	}
	if info.ReaderName != "b" || info.RegisterName != "a" || info.EmitterName != "a" {
		t.Errorf("info = %+v, want b reading a from a", info)
	}
}

// ★ GUARD (ADR-0091 item 9). Under linear dispatch every requisite whose
// source runs in another step is resolved from the audit log, so the detector that
// decides whether the Keeper needs one must see a default block's children as the
// steps they are. The staged detector reads the block as one entry and misses the
// link between them: run without an audit log, the consumer is skipped silently.
//
// Mutation: answer for a linear run with [CrossPassageRequisite] → the default
// block's link is not reported.
func TestCrossStepRequisite_SeesDefaultBlockChildren(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		cross    bool
		consumer string
	}{
		{
			name: "sibling children of a default block",
			src: `
name: linear
tasks:
  - name: pair
    block:
      - name: write config
        module: core.exec.run
        register: cfg
        params: { cmd: write }
      - name: reload
        module: core.exec.run
        onchanges: [cfg]
        params: { cmd: reload }
`,
			cross:    true,
			consumer: "reload",
		},
		{
			name: "a requisite on a default block reaches its children",
			src: `
name: linear
tasks:
  - name: write config
    module: core.exec.run
    register: cfg
    params: { cmd: write }
  - name: on change
    onchanges: [cfg]
    block:
      - name: reload
        module: core.exec.run
        params: { cmd: reload }
`,
			cross:    true,
			consumer: "reload",
		},
		{
			name: "sibling children of a group travel in one message",
			src: `
name: linear
tasks:
  - name: canary
    serial: 1
    settings:
      serial_scope: block
    block:
      - name: write config
        module: core.exec.run
        register: cfg
        params: { cmd: write }
      - name: reload
        module: core.exec.run
        onchanges: [cfg]
        params: { cmd: reload }
`,
		},
		{
			name: "no requisite",
			src:  linearOverlayAndGroup,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadLinearManifest(t, tc.src)
			plan, steps := LinearPassage(m.Tasks, m.Settings)
			info, bad := CrossStepRequisite(m.Tasks, steps)
			if bad != tc.cross {
				t.Fatalf("CrossStepRequisite = %v (%+v), want %v", bad, info, tc.cross)
			}
			if !tc.cross {
				return
			}
			if info.ConsumerName != tc.consumer || info.RequisiteName != "cfg" || info.Kind != "onchanges" {
				t.Errorf("info = %+v, want %q reading cfg through onchanges", info, tc.consumer)
			}
			if info.SourcePassage >= info.ConsumerPassage {
				t.Errorf("info = %+v, want the source step before the consumer's", info)
			}
			if tc.name == "sibling children of a default block" {
				if _, staged := CrossPassageRequisite(m.Tasks, plan); staged {
					t.Error("the staged detector now sees a default block's children - the fixture no longer shows what it misses")
				}
			}
		})
	}
}

// ★ GUARD (rescues.md, fork 1). The steps a run that a failure ended still walks to
// are the ones holding an `onfail:` whose source ran at an earlier step: a top-level
// rescue, a block's `onfail:` reaching each child step, but not a rescue inside the
// group its source sits in, nor an `onchanges:`, nor a keeper-side `onfail:` task.
//
// Mutations: count a same-step source → the group's own rescue makes step 4 a rescue
// step; read every requisite kind → `notify` makes step 5 one; count a keeper-side
// task → `record` makes step 6 one.
func TestRescueSteps_OnlyCrossStepOnfail(t *testing.T) {
	m := loadLinearManifest(t, `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: rollback
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: rollback }
  - name: cleanup
    onfail: [deploy]
    block:
      - name: undo
        module: core.exec.run
        params: { cmd: undo }
      - name: tidy
        module: core.exec.run
        params: { cmd: tidy }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: change
        module: core.exec.run
        register: change
        params: { cmd: change }
      - name: revert
        module: core.exec.run
        onfail: [change]
        params: { cmd: revert }
  - name: notify
    module: core.exec.run
    onchanges: [deploy]
    params: { cmd: notify }
  - name: record
    module: core.soul.registered
    onfail: [deploy]
    params: { sid: host-new.example.com }
`)
	_, steps := LinearPassage(m.Tasks, m.Settings)
	want := map[int][]string{1: {"deploy"}, 2: {"deploy"}, 3: {"deploy"}}
	if got := RescueSteps(m.Tasks, steps); !reflect.DeepEqual(got, want) {
		t.Errorf("RescueSteps = %v, want %v", got, want)
	}
	if got := RegisterSteps(m.Tasks, steps); !reflect.DeepEqual(got, map[string][]int{"deploy": {0}, "change": {4}}) {
		t.Errorf("RegisterSteps = %v, want deploy at 0 and the group's change at its step 4", got)
	}
}

// steppedPlan is a default block holding a nested one, a group and a loop: steps 0,
// 1-3 (the outer block), 2-3 (the inner), 4 (the group) and 5 (the loop).
const steppedPlan = `
name: linear
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: overlay
    block:
      - name: a
        module: core.exec.run
        register: a
        params: { cmd: a }
      - name: inner
        block:
          - name: b
            module: core.exec.run
            register: b
            params: { cmd: b }
          - name: c
            module: core.exec.run
            params: { cmd: c }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: change
        module: core.exec.run
        register: change
        params: { cmd: change }
      - name: check
        module: core.exec.run
        register: check
        params: { cmd: check }
  - name: each
    module: core.exec.run
    register: each
    loop:
      items: "${ ['x', 'y'] }"
    params: { cmd: "${ item }" }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1). Each register stands at the step of
// the task that sets it: a default block's child at its own step, at any depth, a
// group's child at the group's one step, a loop at its one step whatever its items.
//
// Mutation: number a group's children past the group (collectStepLinks gives a
// child of a node without children a step of its own) → change and check move.
func TestRegisterSteps_EachRegisterAtItsSettersStep(t *testing.T) {
	m := loadLinearManifest(t, steppedPlan)
	_, steps := LinearPassage(m.Tasks, m.Settings)
	want := map[string][]int{"probe": {0}, "a": {1}, "b": {2}, "change": {4}, "check": {4}, "each": {5}}
	if got := RegisterSteps(m.Tasks, steps); !reflect.DeepEqual(got, want) {
		t.Errorf("RegisterSteps = %v, want %v", got, want)
	}
	if got := RegisterSteps(m.Tasks, steps[:1]); got != nil {
		t.Errorf("RegisterSteps over another plan's shape = %v, want nil", got)
	}

	m = loadLinearManifest(t, linearOverlayAndGroup)
	_, steps = LinearPassage(m.Tasks, m.Settings)
	if got := RegisterSteps(m.Tasks, steps); !reflect.DeepEqual(got, map[string][]int{"probe": {0}, "a": {1}}) {
		t.Errorf("RegisterSteps = %v, want probe at 0 and a at 1", got)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rules 1-4). A construct spans from its first
// step to its last: a default block to its last descendant's, at any depth; a task, a
// group and anything inside a group are one step. Below any other task there is no
// position.
//
// Mutations: give a default block its first step as its last → overlay spans 1-1;
// answer a position below a task that is no group with the task's step → 0/3 and
// 1/0/0 are found.
func TestStepSpan_ConstructsAndTheirSteps(t *testing.T) {
	m := loadLinearManifest(t, steppedPlan)
	_, steps := LinearPassage(m.Tasks, m.Settings)
	for _, tc := range []struct {
		path        string
		first, last int
		ok          bool
	}{
		{"0", 0, 0, true},
		{"1", 1, 3, true},
		{"1/0", 1, 1, true},
		{"1/1", 2, 3, true},
		{"1/1/1", 3, 3, true},
		{"2", 4, 4, true},
		{"2/1", 4, 4, true},
		{"3", 5, 5, true},
		{"4", 0, 0, false},
		{"1/2", 0, 0, false},
		{"0/3", 0, 0, false},
		{"1/0/0", 0, 0, false},
		{"3/0", 0, 0, false},
		{"", 0, 0, false},
		{"x", 0, 0, false},
		{"-1", 0, 0, false},
	} {
		first, last, ok := StepSpan(steps, tc.path)
		if ok != tc.ok || (ok && (first != tc.first || last != tc.last)) {
			t.Errorf("StepSpan(%q) = (%d, %d, %v), want (%d, %d, %v)", tc.path, first, last, ok, tc.first, tc.last, tc.ok)
		}
	}
}

const crossStepUnitPlan = `
name: linear
tasks:
  - name: deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: earlier
    module: core.exec.run
    onfail: [deploy]
    params: { cmd: earlier }
  - name: mixed
    onfail: [deploy, verify]
    block:
      - name: r1
        module: core.exec.run
        register: r1
        params: { cmd: r1 }
      - name: r2
        module: core.exec.run
        when: register.r1.stdout == 'ok'
        params: { cmd: r2 }
  - name: later
    module: core.exec.run
    onfail: [verify]
    params: { cmd: later }
  - name: pair
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: stage
        module: core.exec.run
        register: stage
        params: { cmd: stage }
      - name: unstage
        module: core.exec.run
        onfail: [stage, deploy]
        params: { cmd: unstage }
  - name: wrap
    onfail: [deploy]
    block:
      - name: w1
        module: core.exec.run
        params: { cmd: w1 }
      - name: inner
        module: core.exec.run
        onfail: [verify]
        params: { cmd: inner }
  - name: canary
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: s2
        module: core.exec.run
        register: s2
        params: { cmd: s2 }
      - name: fixer
        onfail: [deploy]
        block:
          - name: t
            module: core.exec.run
            onfail: [s2]
            params: { cmd: t }
  - name: app
    register: app
    apply:
      destiny: d
  - name: renew
    module: core.exec.run
    onfail: [app]
    params: { cmd: renew }
  - name: verify
    module: core.exec.run
    register: verify
    params: { cmd: verify }
  - name: ahead
    serial: 2
    settings: { serial_scope: block }
    block:
      - name: fwd
        module: core.exec.run
        onfail: [s3]
        params: { cmd: fwd }
      - name: s3
        module: core.exec.run
        register: s3
        params: { cmd: s3 }
`

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1 "Units with no source before them in
// their own message"). An outermost unit is cross-step when none of its sources is set
// at a step of its own by a task before it, whatever mix of earlier and later steps they
// stand at: all earlier (`earlier`), earlier and later (`mixed`), only later (`later`), a
// nested unit of another source (`wrap`), an applier's register (`renew`), a sibling in
// its group after it (`fwd`, a forward `onfail:` the Soul reaches only after the unit).
// A unit with a source before it in its own group is not (`unstage`), nor is one whose
// nested unit names a sibling in the group around it (`fixer`, though its own `onfail:`
// names only an earlier source). Only the outermost unit is listed. The applier `app` is
// no unit. A plan with no unit, or numbered from another plan, has none.
//
// Mutations: take a unit by its sources at earlier steps only (s >= first refused
// for every source) → mixed, later and wrap drop out; read the outermost construct's
// own `onfail:` alone → fixer (6/1) counts; key every construct naming a source as a
// unit of its own → 5/1 and 6/1 are listed; count an in-span source wherever it stands
// in its group (no s.order < unit in sourcesOutside) → fwd (10/0) drops out.
func TestCrossStepUnits_UnitsWithNoSourceInTheirOwnMessage(t *testing.T) {
	m := loadLinearManifest(t, crossStepUnitPlan)
	_, steps := LinearPassage(m.Tasks, m.Settings)
	want := map[string]bool{"1": true, "2": true, "3": true, "5": true, "8": true, "10/0": true}
	if got := CrossStepUnits(m.Tasks, steps); !reflect.DeepEqual(got, want) {
		t.Errorf("CrossStepUnits = %v, want %v", got, want)
	}

	plain := loadLinearManifest(t, steppedPlan)
	_, plainSteps := LinearPassage(plain.Tasks, plain.Settings)
	if got := CrossStepUnits(plain.Tasks, plainSteps); got != nil {
		t.Errorf("CrossStepUnits of a plan with no unit = %v, want nil", got)
	}
	if got := CrossStepUnits(m.Tasks, plainSteps); got != nil {
		t.Errorf("CrossStepUnits over another plan's steps = %v, want nil", got)
	}
}
