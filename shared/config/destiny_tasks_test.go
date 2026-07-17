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
