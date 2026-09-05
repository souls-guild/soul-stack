package validate

// NIM-783: `validate-destiny` says something about the destiny's `tasks/main.yml`
// — including whether it could check the plugin steps in it.
//
// The third file of this shape (NIM-778 shipped through it, NIM-779 closed the
// scenario-include half): the command declares `--modules`, the task file was read
// twice for cross-file facts, and both reads threw the verdict away. So an
// undeclared param printed nothing — not `unknown_param`, and not the
// `plugin_params_unchecked` hint that exists to say "this could not be checked".
//
// The subject is the WIRING. That the walk finds a step inside a `block:`, or two
// levels down an include chain, is [config]'s to prove and it does; what these pin
// is that `validate-destiny` reaches the file at all, carries `--modules` into it and
// into its includes, and never goes quiet.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Soul-side module with one required param — a destiny is Soul-side by
// construction, so a keeper-side fixture would be refused for a different reason
// and prove nothing about params.
const instanceSchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"instance","side":"soul","description":"a live instance","states":{"configured":{` +
	`"description":"the instance is configured","input":{` +
	`"addr":{"type":"string","required":true},"name":{"type":"string"}}}}}]}`

const (
	// goodStepParams matches the schema; bogusStepParams adds a param the module
	// does not declare — the mutation the silent path let through.
	goodStepParams  = "    addr: 127.0.0.1:6379\n    name: primary\n"
	bogusStepParams = "    addr: 127.0.0.1:6379\n    bogus_param: \"x\"\n"
)

// destinyTree writes a destiny artifact whose only plugin step sits in the task file
// named by `where` (`tasks/main.yml` inline, or `tasks/record.yml` behind an
// include). params is spliced into that step. It returns the path to destiny.yml.
func destinyTree(t *testing.T, where, params string) string {
	t.Helper()
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "destiny.yml"),
		"name: probe\ndescription: a destiny whose task file carries a plugin step\n")

	step := "- name: configure the instance\n  module: redis.instance.configured\n  params:\n" + params
	switch where {
	case "main":
		stageWrite(t, filepath.Join(root, "tasks", "main.yml"), step)
	case "include":
		stageWrite(t, filepath.Join(root, "tasks", "main.yml"), "- include: record.yml\n")
		stageWrite(t, filepath.Join(root, "tasks", "record.yml"), step)
	default:
		t.Fatalf("unknown fixture shape %q", where)
	}
	return filepath.Join(root, "destiny.yml")
}

// runDestiny is `soul-lint validate-destiny` over one manifest.
func runDestiny(t *testing.T, manifest string, modules []string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: manifest, Kind: KindDestiny, Modules: modules}, &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("soul-lint failed to run: %s", errOut.String())
	}
	return code, out.String()
}

// bindInstance writes the module document and returns the `--modules` binding.
func bindInstance(t *testing.T) []string {
	t.Helper()
	return []string{"redis=" + writeSchemaFile(t, t.TempDir(), "soul-mod-redis", instanceSchemaJSON)}
}

// THE guard. No manifest bound, so the params genuinely cannot be checked — and
// that is a thing to SAY. A run that prints nothing here has told the author the
// step is fine, at both depths.
func TestDestinyTasks_PluginStepUncheckedIsReportedNotSilent(t *testing.T) {
	for _, tc := range []struct{ shape, file string }{
		{"main", filepath.Join("tasks", "main.yml")},
		{"include", filepath.Join("tasks", "record.yml")},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			manifest := destinyTree(t, tc.shape, goodStepParams)

			code, out := runDestiny(t, manifest, nil)

			// A hint, so the run still passes: the author of a destiny usually cannot
			// produce somebody else's plugin manifest.
			if code != ExitOK {
				t.Fatalf("exit = %d, want %d — an unbound manifest is a hint, not a failure\n%s", code, ExitOK, out)
			}
			if !containsDiagFor(out, tc.file, "plugin_params_unchecked") {
				t.Fatalf("silence: nothing said about the plugin step, at the file that carries it\n%s", out)
			}
		})
	}
}

// And with the manifest bound the check is REAL — the same `unknown_param` a
// scenario's step gets, at the task file's own line, and the run fails.
func TestDestinyTasks_PluginParamsAreCheckedAgainstTheManifest(t *testing.T) {
	for _, tc := range []struct{ shape, file string }{
		{"main", filepath.Join("tasks", "main.yml")},
		{"include", filepath.Join("tasks", "record.yml")},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			manifest := destinyTree(t, tc.shape, bogusStepParams)

			code, out := runDestiny(t, manifest, bindInstance(t))

			if code != ExitHasErrors {
				t.Fatalf("exit = %d, want %d — an undeclared param must fail the lint\n%s", code, ExitHasErrors, out)
			}
			if !containsDiagFor(out, tc.file, "unknown_param") {
				t.Errorf("the undeclared param is not reported at its own file\n%s", out)
			}
			if strings.Contains(out, "plugin_params_unchecked") {
				t.Errorf("reported the step as unchecked while checking it\n%s", out)
			}
		})
	}
}

// The required-param half: a param the module demands is simply absent.
func TestDestinyTasks_MissingRequiredParamIsReported(t *testing.T) {
	manifest := destinyTree(t, "main", "    name: primary\n")

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "main.yml"), "missing_required_param") {
		t.Errorf("a required param missing from a destiny step was accepted\n%s", out)
	}
}

// A correct step against a bound manifest is quiet — neither the hint nor a
// finding. Without this the two above pass on an implementation that reports
// something unconditionally.
func TestDestinyTasks_CheckedAndCleanIsQuiet(t *testing.T) {
	manifest := destinyTree(t, "main", goodStepParams)

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}
	for _, c := range []string{"plugin_params_unchecked", "unknown_param", "missing_required_param", "destiny_tasks_unchecked"} {
		if strings.Contains(out, c) {
			t.Errorf("a step matching its manifest produced %s\n%s", c, out)
		}
	}
}

// No task file at all is the other way the check does not run, and it gets the same
// treatment: a hint that names the reason, not an `OK:` on its own. A destiny
// without `tasks/main.yml` does not load at the keeper either.
func TestDestinyTasks_MissingTaskFileIsReportedNotSilent(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a manifest with no task tree beside it\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d — an absent task file is a hint, not a failure\n%s", code, ExitOK, out)
	}
	if !containsDiagFor(out, "destiny.yml", "destiny_tasks_unchecked") {
		t.Fatalf("silence: nothing said about a destiny whose tasks were never opened\n%s", out)
	}
}

// The task file is loaded under the same `DestinyTasks` flag the keeper's loader
// sets, so a keeper-side address in it is refused offline instead of at dispatch.
// This is what makes the pass a preview of the keeper's own verdict rather than a
// second opinion about the file.
func TestDestinyTasks_KeeperSideModuleIsRefusedOffline(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a destiny reaching for a keeper-side module\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: capture\n  module: core.state.present\n  params:\n    field: greeting\n    value: hi\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "main.yml"), "keeper_module_in_destiny") {
		t.Errorf("a keeper-side address in a destiny's task file passed the lint\n%s", out)
	}
}

// An include that does not resolve is an ERROR here, not a deferral. A destiny is
// its own git artifact — there is no second level offline and none at run time
// either, so unlike a loose scenario there is nothing the keeper could resolve later
// (NIM-716's downgrade must not spread to this path).
func TestDestinyTasks_UnresolvableIncludeIsAnError(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a destiny including a file that is not there\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"), "- include: absent.yml\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !strings.Contains(out, "include_resolve_failed") {
		t.Errorf("an include with no target passed the lint\n%s", out)
	}
}

// Binding `--modules` must not SHRINK the report. An undeclared param is a
// semantic error and leaves the task list intact, so the include tree is still worth
// expanding — bailing on severity would mean the strictest invocation says the least,
// and the unresolvable `include:` beside the bad step would vanish exactly when the
// author asked for more checking.
func TestDestinyTasks_BindingModulesDoesNotHideTheIncludeTree(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: one bad step and one bad include\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: configure the instance\n  module: redis.instance.configured\n  params:\n"+bogusStepParams+
			"\n- include: absent.yml\n")

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !strings.Contains(out, "[unknown_param]") {
		t.Errorf("the bad param is missing\n%s", out)
	}
	if !strings.Contains(out, "include_resolve_failed") {
		t.Errorf("the unresolvable include disappeared once --modules was bound\n%s", out)
	}
}

// A task file that IS there and will not open is an ERROR, not the absent-file hint.
// The hint's argument — "an author is linting a fragment, not an artifact root" —
// does not cover a neighbour that returns EACCES, and exiting 0 over a destiny the
// keeper cannot load would be the false green in a new costume.
func TestDestinyTasks_UnreadableTaskFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is still readable")
	}
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a task file that will not open\n")
	tasks := filepath.Join(root, "tasks", "main.yml")
	stageWrite(t, tasks, "- name: t\n  module: core.file.present\n  params:\n    path: /tmp/x\n")
	if err := os.Chmod(tasks, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tasks, 0o644) })

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d — an unreadable task file must not pass\n%s", code, ExitHasErrors, out)
	}
	if strings.Contains(out, "destiny_tasks_unchecked") {
		t.Errorf("an unreadable file was reported as an absent one\n%s", out)
	}
}

// The securejoin root is the destiny's ROOT, not its `tasks/` directory, and that is
// the resolver's whole load-bearing claim: securejoin re-roots an escaping symlink at
// whatever root it is given, so rooting one tier lower does not narrow what resolves
// — it silently reads a DIFFERENT file than the keeper will.
//
// `tasks/link.yml -> /decoy/body.yml` must therefore resolve to `<root>/decoy/body.yml`,
// which is what the keeper does against its snapshot root. Without this the mutation is
// invisible: every other test here passes under the wrong root.
func TestDestinyTasks_EscapingSymlinkReRootsAtTheArtifactRoot(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: an include reached through an escaping symlink\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"), "- include: link.yml\n")
	// Under the artifact root: what the keeper would read.
	stageWrite(t, filepath.Join(root, "decoy", "body.yml"),
		"- name: the file at the artifact root\n  module: redis.instance.configured\n  params:\n"+bogusStepParams)
	// Under `tasks/`: what a resolver rooted one tier too low would read instead.
	stageWrite(t, filepath.Join(root, "tasks", "decoy", "body.yml"),
		"- name: the file under tasks\n  module: core.file.present\n  params:\n    path: /tmp/x\n")
	if err := os.Symlink("/decoy/body.yml", filepath.Join(root, "tasks", "link.yml")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d — the body at the artifact root has a bad param\n%s", code, ExitHasErrors, out)
	}
	if !strings.Contains(out, "[unknown_param]") {
		t.Errorf("the resolver read the wrong file: the body under the artifact root was never checked\n%s", out)
	}
}

// The two forms the include grammar allows (destiny/tasks.md §4): a flat name and one
// subdirectory down. Every other fixture here uses the flat one.
func TestDestinyTasks_IncludeOneSubdirectoryDownIsChecked(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a body one subdirectory down\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"), "- include: sub/body.yml\n")
	stageWrite(t, filepath.Join(root, "tasks", "sub", "body.yml"),
		"- name: configure\n  module: redis.instance.configured\n  params:\n"+bogusStepParams)

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "sub", "body.yml"), "unknown_param") {
		t.Errorf("a body one subdirectory down was not checked\n%s", out)
	}
}

// A CONDITIONAL include, which is what the corpus destinies are actually made of
// (`examples/destiny/redis/tasks/main.yml` is five of them), and a second level below
// it. A gate on the include does not exempt the body from being read: expansion
// stamps the group for render-time drop, it does not skip the file.
func TestDestinyTasks_ConditionalAndNestedIncludesAreCheckedToo(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a gated include over a nested one\n"+
		"input:\n  enabled:\n    type: boolean\n    required: false\n    default: true\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- include: outer.yml\n  when: \"default(input.enabled, true)\"\n")
	stageWrite(t, filepath.Join(root, "tasks", "outer.yml"), "- include: inner.yml\n")
	stageWrite(t, filepath.Join(root, "tasks", "inner.yml"),
		"- name: configure\n  module: redis.instance.configured\n  params:\n"+bogusStepParams)

	code, out := runDestiny(t, manifest, bindInstance(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "inner.yml"), "unknown_param") {
		t.Errorf("a body two levels down behind a conditional include was not checked\n%s", out)
	}
}

// A cross-file finding — one the flattened plan produces and no single file can —
// lands on the entry point rather than on `<input>`. Expansion has erased AST
// positions by then, so the diagnostic arrives with no File at all and the pass has
// to attribute it.
func TestDestinyTasks_CrossFileFindingIsAttributedToTheEntryPoint(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: one register declared in two files\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: probe here\n  module: core.exec.run\n  params:\n    cmd: /bin/true\n  register: probe\n\n- include: body.yml\n")
	stageWrite(t, filepath.Join(root, "tasks", "body.yml"),
		"- name: probe again\n  module: core.exec.run\n  params:\n    cmd: /bin/true\n  register: probe\n")

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "main.yml"), "duplicate_task_address") {
		t.Errorf("the cross-file duplicate lost its file and printed against <input>\n%s", out)
	}
}

// The manifest's own checks did not regress, and the new pass does not swallow them:
// a broken `destiny.yml` still fails on its own account while its task file is
// checked beside it.
func TestDestinyTasks_ManifestDiagnosticsSurvive(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "destiny.yml")
	stageWrite(t, manifest, "name: probe\ndescription: a manifest with an unknown top-level key\nnot_a_destiny_key: 1\n")
	stageWrite(t, filepath.Join(root, "tasks", "main.yml"),
		"- name: configure the instance\n  module: redis.instance.configured\n  params:\n"+goodStepParams)

	code, out := runDestiny(t, manifest, nil)

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, "destiny.yml", "unknown_key") {
		t.Errorf("the manifest's own finding was lost\n%s", out)
	}
	if !containsDiagFor(out, filepath.Join("tasks", "main.yml"), "plugin_params_unchecked") {
		t.Errorf("the task file was skipped because the manifest was broken\n%s", out)
	}
}
