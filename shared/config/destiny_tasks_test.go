package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadDestinyTasks_Flat(t *testing.T) {
	src := `
- name: Lay down the marker file
  module: core.file.present
  params:
    path: "${ input.marker_file }"
    content: "${ input.marker_payload }"
- name: Record placement
  module: core.exec.run
  changed_when: "false"
  params:
    cmd: "echo ${ input.marker_file }"
`
	tasks, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(tasks))
	}
	if tasks[0].Module == nil || tasks[0].Module.Module != "core.file.present" {
		t.Errorf("task0 module = %+v", tasks[0].Module)
	}
	if tasks[1].Module == nil || tasks[1].Module.Module != "core.exec.run" {
		t.Errorf("task1 module = %+v", tasks[1].Module)
	}
}

func TestLoadDestinyTasks_NotSequence(t *testing.T) {
	// A manifest wrapper (mapping) where a flat list is expected → type_mismatch.
	src := "name: pilot-flat\ntasks: []\n"
	_, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected a type_mismatch diagnostic for mapping instead of sequence")
	}
}

func TestLoadDestinyTasks_Empty(t *testing.T) {
	_, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte("\n"), ValidateOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected an empty_document diagnostic for an empty file")
	}
}

func TestLoadDestinyTasks_BadTask(t *testing.T) {
	// A task with no module/apply/include/block discriminator.
	src := "- name: orphan\n  params: {}\n"
	_, diags, err := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected a diagnostic for a task without a discriminator")
	}
}

// TestLoadDestinyTasks_KeeperModuleInDestiny — a destiny's task list cannot carry
// a keeper-side module, and this is the surviving half of the retired
// `state_capture_not_on_keeper` (NIM-749).
//
// A destiny is rendered per host and dispatched to a Soul, so the address is an
// unknown module there and the run dies at that step; `on: keeper` is not a way
// in either (`resolveOn` aborts on the literal). The loud failure is not the
// point — the L0 fold keys on the module ADDRESS, so such a step predicts its
// result exactly as a routed one does and the case goes green on a plan no run
// can execute.
func TestLoadDestinyTasks_KeeperModuleInDestiny(t *testing.T) {
	t.Run("a keeper-side address is refused", func(t *testing.T) {
		src := "- name: capture\n  module: core.state.set\n  params: { field: owner, value: alice }\n"
		_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
		if !hasCodeP(diags, "keeper_module_in_destiny") {
			t.Fatalf("expected keeper_module_in_destiny, got %v", diagCodesP(diags))
		}
	})

	// Generalised while being split out: the old rule named `core.state`, this
	// asks the catalog, so every keeper-side address is covered.
	t.Run("every keeper-side address is covered", func(t *testing.T) {
		for _, addr := range []string{"core.cloud.created", "core.soul.registered", "core.vault.kv-read", "core.choir.present"} {
			src := "- name: t\n  module: " + addr + "\n  params: {}\n"
			_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
			if !hasCodeP(diags, "keeper_module_in_destiny") {
				t.Errorf("%s: expected keeper_module_in_destiny, got %v", addr, diagCodesP(diags))
			}
		}
	})

	// A block's children reach a host by the same route, so the walk recurses.
	t.Run("a block child is refused too", func(t *testing.T) {
		src := "- name: group\n  block:\n    - name: capture\n      module: core.state.set\n      params: { field: owner, value: alice }\n"
		_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
		if !hasCodeP(diags, "keeper_module_in_destiny") {
			t.Fatalf("expected keeper_module_in_destiny for a block child, got %v", diagCodesP(diags))
		}
	})

	// The negative half, and the reason the check is behind a flag at all: the
	// SAME loader serves a scenario's `include:`d body, where a keeper-side
	// address is the ordinary, correct way to write the step.
	t.Run("an included scenario body is untouched", func(t *testing.T) {
		src := "- name: capture\n  module: core.state.set\n  params: { field: owner, value: alice }\n"
		_, diags, _ := LoadDestinyTasksFromBytes("_create/capture.yml", []byte(src), ValidateOptions{})
		if hasCodeP(diags, "keeper_module_in_destiny") {
			t.Fatalf("a scenario's included body must keep keeper-side addresses, got %v", diagCodesP(diags))
		}
	})

	// And a Soul-side module in a destiny is what a destiny is made of.
	t.Run("a Soul-side address is untouched", func(t *testing.T) {
		src := "- name: t\n  module: core.pkg.installed\n  params: { name: redis }\n"
		_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{DestinyTasks: true})
		if hasCodeP(diags, "keeper_module_in_destiny") {
			t.Fatalf("a Soul-side module belongs in a destiny, got %v", diagCodesP(diags))
		}
	})
}
