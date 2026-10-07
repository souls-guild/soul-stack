package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Step numbering for linear dispatch (draft ADR NIM-907, §`passage`, fork 3): a
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
