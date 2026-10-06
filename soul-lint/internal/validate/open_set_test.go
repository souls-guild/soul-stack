package validate

import (
	"path/filepath"
	"testing"
)

// TestOpenSetRefusals_ReachTheLinter — the two parse-time refusals of a
// Keeper-rendered predicate (NIM-928, shared/config/task_refs_open_set.go) are made by
// the scenario loader the keeper also loads with, so the linter reports them with no
// rule of its own. This pins that they reach `soul-lint` output, each on the file that
// holds it — a loader the linter stopped calling would leave the author to meet them
// mid-run.
func TestOpenSetRefusals_ReachTheLinter(t *testing.T) {
	root := writeMinimalTree(t)
	writeFile(t, filepath.Join(root, "scenario", "check", "main.yml"),
		"name: check\ndescription: Open set\n\ntasks:\n"+
			"  - name: Named like the open set\n    module: core.exec.run\n    register: self\n    params:\n      cmd: 'true'\n"+
			"  - name: Own result before the module\n    module: core.exec.run\n    params:\n      cmd: 'true'\n    when: register.self.rc == 0\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitHasErrors, out)
	}
	file := filepath.Join("scenario", "check", "main.yml")
	for _, c := range []string{"register_name_reserved", "when_register_self_unsupported"} {
		if !containsDiagFor(out, file, c) {
			t.Errorf("%s missing for %s:\n%s", c, file, out)
		}
	}
}
