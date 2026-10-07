package validate

import (
	"path/filepath"
	"strings"
	"testing"
)

// NIM-941 refuses `async:`/`require:` outside a destiny; the scenario half is the
// `scenario-broken/scenario-*-outside-destiny` fixtures. This is the other half:
// `validate-destiny` reaches the task file with the destiny flag set, so neither key
// is refused in it, inside a block, or one include down.
func TestDestinyTasks_AsyncAndRequireAreAccepted(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a destiny joining async tasks\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: warm\n  module: core.exec.run\n  register: warm\n  async: true\n  params:\n    cmd: /bin/true\n"+
			"- block:\n    - name: verify\n      module: core.exec.run\n      require: [warm]\n      params:\n        cmd: /bin/true\n"+
			"- include: body.yml\n")
	stageWrite(t, filepath.Join(root, "tasks", "body.yml"),
		"- name: fetch\n  module: core.exec.run\n  register: fetch\n  async: true\n  params:\n    cmd: /bin/true\n"+
			"- name: report\n  module: core.exec.run\n  require: all\n  params:\n    cmd: /bin/true\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}
	for _, c := range []string{"async_outside_destiny", "require_outside_destiny"} {
		if strings.Contains(out, c) {
			t.Errorf("a destiny's task file drew %s\n%s", c, out)
		}
	}
}

// A destiny is the only place `require:` is legal, so it is where its name checks
// still speak: a barrier on a task that starts later is refused offline.
func TestDestinyTasks_ForwardRequireIsRefused(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a destiny waiting on a later task\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: restart\n  module: core.service.restarted\n  require: [conf]\n  params:\n    name: nginx\n"+
			"- name: render\n  module: core.exec.run\n  register: conf\n  async: true\n  params:\n    cmd: /bin/true\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "main.yml"), "require_forward_reference") {
		t.Errorf("a forward require: in a destiny passed the lint\n%s", out)
	}
}
