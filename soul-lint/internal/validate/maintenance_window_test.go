package validate

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// in_maintenance_window() offline (ADR-0093 §8): refused where an expression is
// evaluated after the Keeper renders the step, accepted where the Keeper renders it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop a lateMaintenance call
// from computeChecker.tasks or captureMatch, or the destinyMaintenanceDiagnostics
// call from destinyTasksDiagnostics — the matching case goes red.
func TestMaintenanceWindow_LateContextsAreReported(t *testing.T) {
	call := "in_maintenance_window()"
	gated := execTask("gated")
	gated.When = call
	gated.Module.Params["cmd"] = "echo ${ " + call + " }"
	late := execTask("late")
	late.ChangedWhen = call
	late.FailedWhen = "!" + call
	late.Retry = &config.RetrySpec{Until: call}
	capture := config.Task{Name: "capture", Module: &config.ModuleTask{Module: "core.state.add", Params: map[string]any{
		"field": "users", "value": "x", "match": "elem == value && " + call,
	}}}

	rendered := config.Task{Name: "rendered", Module: &config.ModuleTask{Module: "core.state.add", Params: map[string]any{
		"field": "users", "value": "x", "match": "${ " + call + " } && elem == value",
	}}}
	mixed := config.Task{Name: "mixed", Module: &config.ModuleTask{Module: "core.state.add", Params: map[string]any{
		"field": "users", "value": "x", "match": "${ input.x } == elem && " + call,
	}}}

	scn := computeScn([]config.Task{gated, late, capture, rendered, mixed})
	scn.Compute = append(scn.Compute,
		config.ComputeVar{Name: "open", Value: "${ " + call + " }"},
		config.ComputeVar{Name: "next", Value: "${ incarnation.maintenance_window.next_start }"})

	var got []string
	for _, d := range computeDiagnostics("scn.yml", scn, true) {
		if d.Code == codeMaintenanceWindowOutOfScope {
			got = append(got, d.YAMLPath)
		}
	}
	want := []string{"$.compute.open", "$.compute.next", "$.tasks[1].changed_when", "$.tasks[1].failed_when", "$.tasks[1].retry.until", "$.tasks[2].params.match", "$.tasks[4].params.match"}
	if !slices.Equal(got, want) {
		t.Errorf("reported at %v, want %v", got, want)
	}
}

func TestMaintenanceWindow_DestinyPredicateIsReported(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "destiny.yml"), "name: probe\ndescription: a destiny gated on the window\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: restart\n  module: core.exec.run\n  when: in_maintenance_window()\n  params:\n    cmd: restart\n")
	code, out := runDestiny(t, filepath.Join(root, "destiny.yml"), nil)
	if code == ExitOK || !containsDiagFor(out, filepath.Join("tasks", "main.yml"), codeMaintenanceWindowOutOfScope) {
		t.Fatalf("exit %d, want a %s error at tasks/main.yml\n%s", code, codeMaintenanceWindowOutOfScope, out)
	}
}

// Service vars resolve once per run: the vars check reports the call through the
// service environment's own refusal. A scenario's `vars:` is per step, and accepted.
func TestMaintenanceWindow_ServiceVarsRefuseScenarioVarsAccept(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"vars/00-base.yaml": "open: \"${ in_maintenance_window() }\"\n",
	})
	got := withCode(serviceDiags(t, root, "redis"), codeVarInvalid)
	if len(got) != 1 || !strings.Contains(got[0].Message, "resolve once per run") {
		t.Errorf("want one var_invalid naming the once-per-run refusal, got %+v", got)
	}

	root = writeServiceTree(t, map[string]string{
		"vars/00-base.yaml":        "port: 6379\n",
		"scenario/create/main.yml": scenarioWithTaskVars("  open: \"${ in_maintenance_window() }\"\n", "      x: \"${ vars.open }\"\n"),
	})
	if got := withCode(scenarioDiags(t, root, "create"), codeVarInvalid); len(got) != 0 {
		t.Errorf("a scenario vars: definition calling the function: got %+v, want none", got)
	}
}
