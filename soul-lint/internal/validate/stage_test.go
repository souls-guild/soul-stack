package validate

// NIM-694 guards for the offline include resolve: the linter must expand
// SERVICE-LEVEL includes (`scenario/<file>`, incl. the shared-bodies directory
// `scenario/_create/<file>`), so the stage graph it checks is the graph the
// keeper will run — and a target that resolves nowhere must fail the lint
// instead of being deferred to the keeper as a hint.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const stageLocalTask = `
  - name: local step
    module: core.service.restarted
    params:
      name: redis-server
`

const stageSharedBody = `- name: shared step one
  module: core.service.restarted
  params:
    name: redis-server

- name: shared step two
  module: core.service.restarted
  params:
    name: redis-sentinel
`

// stageWrite writes content at path, creating the parent directories.
func stageWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// stageParse parses a scenario main.yml from disk the way validate does before
// calling stageDiagnostics.
func stageParse(t *testing.T, path string) *config.ScenarioManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	m, _, diags, err := config.LoadScenarioManifestFromBytes(path, data, config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("scenario must parse: err=%v diags=%+v", err, diags)
	}
	return m
}

func hasDiagCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func diagWithCode(diags []diag.Diagnostic, code string) *diag.Diagnostic {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

// TestStageDiagnostics_SharedDirIncludeResolves — the target layout of NIM-694:
// the common bodies of a scenario family live in `scenario/_create/`, so the
// include has to travel BOTH one level up (service level) and one level down
// (the shared directory). The witness is the task count in the passage plan:
// with the include unexpanded it would be 1, not 3.
func TestStageDiagnostics_SharedDirIncludeResolves(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "provision.yml"), stageSharedBody)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/provision.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if diag.HasErrors(diags) {
		t.Fatalf("service-level include must resolve offline, got %+v", diags)
	}
	if hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("include resolved, the offline-deferral hint must be gone: %+v", diags)
	}
	plan := diagWithCode(diags, "passage_plan")
	if plan == nil {
		t.Fatalf("no passage_plan hint: %+v", diags)
	}
	if !strings.Contains(plan.Message, "(3 tasks") {
		t.Fatalf("the included tasks did not reach the stage graph: %q", plan.Message)
	}
}

// TestStageDiagnostics_LocalIncludeShadowsServiceLevel — the local file wins,
// exactly as in the keeper (orchestration.md §6, shadowing with no merge). The
// witness is again the count: the local body has one task, the service-level
// decoy two.
func TestStageDiagnostics_LocalIncludeShadowsServiceLevel(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "provision.yml"), stageSharedBody)
	stageWrite(t, filepath.Join(root, "scenario", "create", "_create", "provision.yml"),
		"- name: local body\n  module: core.service.restarted\n  params:\n    name: redis-server\n")
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/provision.yml\n")

	diags := stageDiagnostics(main, stageParse(t, main))
	if diag.HasErrors(diags) {
		t.Fatalf("include must resolve, got %+v", diags)
	}
	plan := diagWithCode(diags, "passage_plan")
	if plan == nil || !strings.Contains(plan.Message, "(1 tasks") {
		t.Fatalf("local file must shadow the service-level one: %+v", diags)
	}
}

// TestStageDiagnostics_UnresolvedIncludeIsError — inside a service tree BOTH
// levels are on disk, so "not found" is a real defect: an ERROR that fails the
// lint, not the stage_include_unresolved hint that used to let it exit OK.
func TestStageDiagnostics_UnresolvedIncludeIsError(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/missing.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if !hasDiagCode(diags, "include_resolve_failed") {
		t.Fatalf("want include_resolve_failed, got %+v", diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatalf("an include that resolves at neither level must fail the lint: %+v", diags)
	}
	if hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("inside a service tree the failure is real, not a deferral: %+v", diags)
	}
	// The stage graph of a truncated task list is not reported: it describes a
	// plan that never existed.
	if hasDiagCode(diags, "passage_plan") {
		t.Fatalf("a passage plan over a half-expanded list must not be reported: %+v", diags)
	}
}

// TestStageDiagnostics_DynamicIncludeWhenReportedOnce — the expander is the only
// producer of include_when_dynamic_unsupported. There used to be a second one, a
// pre-pass over m.Tasks, and the code was filtered out of the expander's
// pass-through so the author would not read one defect twice; that filter is what
// ate the nested case (see the sibling test below). With the pre-pass gone the
// count must still be exactly one — a reintroduced pre-pass would double it.
func TestStageDiagnostics_DynamicIncludeWhenReportedOnce(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "provision.yml"), stageSharedBody)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/provision.yml\n"+
		"    when: soulprint.self.os.family == 'debian'\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	n := 0
	for _, d := range diags {
		if d.Code == "include_when_dynamic_unsupported" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("include_when_dynamic_unsupported reported %d times, want exactly 1: %+v", n, diags)
	}
}

// TestStageDiagnostics_SymlinkEscapeRefused — the include target is a symlink
// checked into the service tree that points OUT of it. The grammar cannot help
// here: `_create/body.yml` is a perfectly well-formed name, and the escape lives
// in the file system, not in the string. Only a clamp that resolves symlinks
// catches it, which is why the resolver reads through securejoin rather than
// joining lexically and calling os.ReadFile.
//
// The stake is linter/keeper parity: the keeper clamps this read, so a linter
// that follows the symlink would bless a plan the keeper refuses to run.
func TestStageDiagnostics_SymlinkEscapeRefused(t *testing.T) {
	outside := t.TempDir()
	body := filepath.Join(outside, "body.yml")
	stageWrite(t, body, stageSharedBody)

	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	link := filepath.Join(root, "scenario", "_create", "body.yml")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(body, link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/body.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if !hasDiagCode(diags, "include_resolve_failed") {
		t.Fatalf("an include whose target symlinks out of the service tree must not resolve, got: %+v", diags)
	}
	// The escaped body must not have reached the plan by any other route.
	for _, d := range diags {
		if strings.Contains(d.Message, "shared step one") {
			t.Fatalf("content from outside the service tree was spliced into the plan: %+v", d)
		}
	}
}

// TestStageDiagnostics_DynamicIncludeWhenNestedReported — the dynamic when: sits
// on an include inside an INCLUDED file, not in main.yml. Nothing in main.yml is
// wrong, so a check that walks only the manifest's own task list (plus block:)
// sees a clean scenario and the linter exits OK on a scenario the keeper refuses
// to run. Only the expander reaches this node, because only the expander follows
// the include.
//
// The guard is the whole point of expanding service-level includes offline: the
// subject being checked has to be the plan that will actually run, not the file
// that happens to be open.
func TestStageDiagnostics_DynamicIncludeWhenNestedReported(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	// probe.yml is pulled in by provision.yml under a dynamic predicate; main.yml
	// itself carries no when: at all.
	stageWrite(t, filepath.Join(root, "scenario", "_create", "probe.yml"), stageSharedBody)
	stageWrite(t, filepath.Join(root, "scenario", "_create", "provision.yml"),
		"- include: _create/probe.yml\n"+
			"  when: soulprint.self.os.family == 'debian'\n")
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: _create/provision.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if !hasDiagCode(diags, "include_when_dynamic_unsupported") {
		t.Fatalf("a dynamic include-when nested inside an included file must be reported, got: %+v", diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatalf("it must be an ERROR, not a hint — the keeper rejects this plan: %+v", diags)
	}
}

// TestStageDiagnostics_LooseFileKeepsHint — a scenario linted OUTSIDE a service
// tree (a single file, a fixture directory) genuinely has no service level
// offline: the failure stays a hint and the lint still exits OK, as before
// NIM-694.
func TestStageDiagnostics_LooseFileKeepsHint(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "fixtures", "conditional-include.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: install.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if diag.HasErrors(diags) {
		t.Fatalf("a loose file must not fail on a service-level include: %+v", diags)
	}
	if !hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("want the stage_include_unresolved hint, got %+v", diags)
	}
}

// NIM-716. Outside a service tree, an error out of a body that DID resolve is
// the body's own error and is reported as one — at the body's coordinates and
// with exit 1. Only a target that was not FOUND is a deferral to the keeper.
//
// The defect this pins: the downgrade used to test nothing but "no service level"
// and "error", so a file sitting right beside the scenario, resolved locally, read,
// and checked by the same code that checks it inside a service tree, came back as
// `include does not resolve offline (block_on_keeper_invalid)` — a sentence whose
// every clause is false — with exit 0 behind it. The author reads that and goes
// looking for a resolution problem that does not exist.
func TestStageDiagnostics_LooseFileReportsIncludeBodyErrors(t *testing.T) {
	root := t.TempDir() // deliberately WITHOUT service.yml: a loose scenario
	body := filepath.Join(root, "capture.yml")
	stageWrite(t, body, "- name: record the topology\n"+
		"  on: keeper\n"+
		"  block:\n"+
		"    - name: the field\n"+
		"      module: core.state.set\n"+
		"      params: { field: mode, value: sentinel }\n")
	main := filepath.Join(root, "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: capture.yml\n")

	diags := stageDiagnostics(main, stageParse(t, main))
	if !diag.HasErrors(diags) {
		t.Fatalf("the include body's own errors were downgraded to hints: %+v", diags)
	}
	if hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("the include resolved and was read - reporting it as unresolved is the lie NIM-716 removes: %+v", diags)
	}
	d := diagWithCode(diags, "block_on_keeper_invalid")
	if d == nil {
		t.Fatalf("want block_on_keeper_invalid from the body, got %+v", diags)
	}
	// The coordinates must be the BODY's, not the scenario's: an error attributed
	// to main.yml sends the author to a file with nothing wrong in it.
	if filepath.Base(d.File) != "capture.yml" {
		t.Errorf("diagnostic file = %q, want the included body capture.yml", d.File)
	}
	if d.Line == 0 {
		t.Errorf("diagnostic carries no line: %+v", *d)
	}
}

// The other half of the same rule, and the reason the downgrade exists at all: a
// target that genuinely cannot be found outside a service tree stays a HINT with
// exit 0, because the missing service level is the linter's blind spot and not
// the author's mistake. Sits beside TestStageDiagnostics_LooseFileKeepsHint,
// which pins the same thing for the plain case; this one pins that narrowing the
// condition to resolve-only did not narrow it to nothing.
func TestStageDiagnostics_LooseFileStillDefersAnUnfoundTarget(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: install.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if diag.HasErrors(diags) {
		t.Fatalf("an unfindable target outside a service tree must stay a hint: %+v", diags)
	}
	if !hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("want the stage_include_unresolved hint, got %+v", diags)
	}
}

// A dynamic `when:` on an include is a property of the include NODE, so it is an
// error wherever the scenario is linted from. It used to be exempted from the
// downgrade by name; after NIM-716 narrowed the downgrade to resolve failures the
// exemption is unnecessary — and this pins that removing it did not put the code
// back under the sweep.
func TestStageDiagnostics_LooseFileKeepsDynamicIncludeWhen(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "install.yml"), stageSharedBody)
	main := filepath.Join(root, "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n"+
		"  - include: install.yml\n"+
		"    when: soulprint.self.os.family == 'debian'\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if !hasDiagCode(diags, "include_when_dynamic_unsupported") {
		t.Fatalf("a dynamic include-when must be reported outside a service tree too: %+v", diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatalf("it must stay an ERROR: %+v", diags)
	}
}

// An upgrade scenario's `include:` resolves out of `scenario/`, NOT out of the
// directory the scenario is sitting in — because that is what the keeper does, and
// the linter's verdict is worth nothing if it disagrees with the engine.
//
// The keeper loads the entry point channel-aware (`upgrade/<slug>/main.yml`,
// scenario.go:57) and then builds the include levels from the scenario NAME alone:
// `path.Join("scenario", scenarioName)` and `"scenario"`
// (keeper/internal/scenario/include.go:26-27, reached from run.go / preflight.go /
// render_host.go with spec.ScenarioName). It never learns which channel the entry
// point came from. So a body next to an upgrade scenario is unreachable at run
// time, and the linter must say so rather than resolve it and bless the definition
// — resolving it is the false green this whole class of defect is made of.
func TestStageDiagnostics_UpgradeIncludeMirrorsTheKeeper(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	// The body an author would naturally write: right beside the upgrade scenario.
	// The keeper will not find it there, so neither may the linter.
	stageWrite(t, filepath.Join(root, "upgrade", "to_v2", "install.yml"), stageSharedBody)
	main := filepath.Join(root, "upgrade", "to_v2", "main.yml")
	stageWrite(t, main, "name: to_v2\nfrom: ['1']\ntasks:\n  - include: install.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if !diag.HasErrors(diags) {
		t.Fatalf("a body the keeper cannot reach was resolved and blessed: %+v", diags)
	}
	if !hasDiagCode(diags, config.CodeIncludeResolveFailed) {
		t.Fatalf("want %s, got %+v", config.CodeIncludeResolveFailed, diags)
	}
	if hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("an upgrade scenario inside a real service tree was treated as a loose file: %+v", diags)
	}
	// The message names both levels it tried, so the author can see WHERE the
	// engine will look rather than guess from "not found".
	d := diagWithCode(diags, config.CodeIncludeResolveFailed)
	for _, want := range []string{filepath.Join("scenario", "to_v2"), "scenario"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("the message does not name the level %q the keeper uses: %q", want, d.Message)
		}
	}
}

// The other side of that rule: a body where the keeper DOES look resolves, from
// both levels, for an upgrade scenario exactly as for a regular one.
func TestStageDiagnostics_UpgradeIncludeResolvesWhereTheKeeperLooks(t *testing.T) {
	for _, tc := range []struct{ name, rel string }{
		{"service level", filepath.Join("scenario", "install.yml")},
		{"local level, keyed on the scenario name", filepath.Join("scenario", "to_v2", "install.yml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
			stageWrite(t, filepath.Join(root, tc.rel), stageSharedBody)
			main := filepath.Join(root, "upgrade", "to_v2", "main.yml")
			stageWrite(t, main, "name: to_v2\nfrom: ['1']\ntasks:\n  - include: install.yml\n"+stageLocalTask)

			diags := stageDiagnostics(main, stageParse(t, main))
			if diag.HasErrors(diags) {
				t.Fatalf("a body at %s must resolve: %+v", tc.rel, diags)
			}
			if hasDiagCode(diags, "stage_include_unresolved") {
				t.Fatalf("an upgrade scenario inside a real service tree was treated as a loose file: %+v", diags)
			}
		})
	}
}

// The same file must lint to the same verdict however the operator addressed it —
// for the upgrade channel too, where the levels are derived rather than taken from
// the file's own directory and so have one more chance to go wrong.
//
// The sibling case above covers `scenario/`; this one exists because the new
// derivation reads the scenario name off `Dir(Dir(abs))` and joins it onto a
// `pathLike`-rendered service level, and both halves are spelling-sensitive:
// `main.yml` addressed from inside its own directory decomposes to "." lexically,
// which would address the channel root instead of the scenario.
func TestStageDiagnostics_UpgradeVerdictIndependentOfPathForm(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	// At the level the engine uses, so every spelling must RESOLVE it...
	stageWrite(t, filepath.Join(root, "scenario", "to_v2", "install.yml"), stageSharedBody)
	// ...and one beside the scenario, which no spelling may resolve.
	stageWrite(t, filepath.Join(root, "upgrade", "to_v2", "decoy.yml"), stageSharedBody)
	stageWrite(t, filepath.Join(root, "upgrade", "to_v2", "main.yml"),
		"name: to_v2\nfrom: ['1']\ntasks:\n  - include: install.yml\n")
	stageWrite(t, filepath.Join(root, "upgrade", "decoyed", "decoy.yml"), stageSharedBody)
	stageWrite(t, filepath.Join(root, "upgrade", "decoyed", "main.yml"),
		"name: decoyed\nfrom: ['1']\ntasks:\n  - include: decoy.yml\n")

	for _, tc := range []struct{ name, cwd, arg string }{
		{"absolute", root, filepath.Join(root, "upgrade", "to_v2", "main.yml")},
		{"from service root", root, filepath.Join("upgrade", "to_v2", "main.yml")},
		{"from the channel dir", filepath.Join(root, "upgrade"), filepath.Join("to_v2", "main.yml")},
		{"from the scenario's own dir", filepath.Join(root, "upgrade", "to_v2"), "main.yml"},
		{"across channels", filepath.Join(root, "scenario"), filepath.Join("..", "upgrade", "to_v2", "main.yml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)

			diags := stageDiagnostics(tc.arg, stageParse(t, tc.arg))
			if diag.HasErrors(diags) {
				t.Fatalf("the engine's own level did not resolve from %q (cwd %q): %+v", tc.arg, tc.cwd, diags)
			}
			if hasDiagCode(diags, "stage_include_unresolved") {
				t.Fatalf("a real service tree was read as a loose file from %q: %+v", tc.arg, diags)
			}
			plan := diagWithCode(diags, "passage_plan")
			if plan == nil || !strings.Contains(plan.Message, "(2 tasks") {
				t.Fatalf("the included body did not reach the stage graph from %q: %+v", tc.arg, diags)
			}
		})
	}

	// And the decoy beside main.yml stays unreachable from every spelling — the
	// half a wrong local level would silently turn green.
	for _, tc := range []struct{ name, cwd, arg string }{
		{"absolute", root, filepath.Join(root, "upgrade", "decoyed", "main.yml")},
		{"from the scenario's own dir", filepath.Join(root, "upgrade", "decoyed"), "main.yml"},
	} {
		t.Run("decoy/"+tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)

			diags := stageDiagnostics(tc.arg, stageParse(t, tc.arg))
			if !hasDiagCode(diags, config.CodeIncludeResolveFailed) {
				t.Fatalf("a body beside the upgrade scenario resolved from %q: %+v", tc.arg, diags)
			}
		})
	}
}

// TestStageDiagnostics_NoServiceManifestNoServiceLevel — the second level is
// claimed only for a REAL service tree (`scenario/` under a root carrying
// service.yml). Without the manifest the directory above is just a neighbour:
// letting it answer an include would splice an unrelated file into the plan,
// which is worse than not resolving at all.
func TestStageDiagnostics_NoServiceManifestNoServiceLevel(t *testing.T) {
	root := t.TempDir() // deliberately WITHOUT service.yml
	stageWrite(t, filepath.Join(root, "scenario", "install.yml"), stageSharedBody)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n  - include: install.yml\n"+stageLocalTask)

	diags := stageDiagnostics(main, stageParse(t, main))
	if diag.HasErrors(diags) {
		t.Fatalf("must not fail, got %+v", diags)
	}
	if !hasDiagCode(diags, "stage_include_unresolved") {
		t.Fatalf("the neighbour file must NOT have answered the include: %+v", diags)
	}
}

// TestStageDiagnostics_VerdictIndependentOfPathForm — the same file on disk must
// lint to the same verdict however the operator addressed it. The service-tree
// detection walks Dir() upwards, which is a LEXICAL operation: `create/main.yml`
// decomposes to base "." (not "scenario") and `main.yml` decomposes to "." as
// well, but there "." is the scenario's own directory rather than the level
// above it. Uncaught, that made the exit code a function of the caller's shell
// history — strict inside the service root, permissive one directory down —
// and CI never saw it because the Makefile glob always emits the long form.
//
// Every other test here builds its path with filepath.Join(t.TempDir(), ...),
// i.e. always absolute and always deep, so none of them can catch this class.
func TestStageDiagnostics_VerdictIndependentOfPathForm(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	// The shared body carries an include that resolves NOWHERE, so a correctly
	// detected service tree must report an error rather than the hint.
	stageWrite(t, filepath.Join(root, "scenario", "_shared", "a.yml"),
		stageSharedBody+"\n- include: nowhere.yml\n")
	stageWrite(t, filepath.Join(root, "scenario", "create", "main.yml"),
		"name: create\ntasks:\n  - include: _shared/a.yml\n"+stageLocalTask)

	for _, tc := range []struct{ name, cwd, arg string }{
		{"absolute", root, filepath.Join(root, "scenario", "create", "main.yml")},
		{"from service root", root, filepath.Join("scenario", "create", "main.yml")},
		{"from scenario dir", filepath.Join(root, "scenario"), filepath.Join("create", "main.yml")},
		{"from the scenario's own dir", filepath.Join(root, "scenario", "create"), "main.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)

			diags := stageDiagnostics(tc.arg, stageParse(t, tc.arg))
			if !diag.HasErrors(diags) {
				t.Fatalf("service tree not detected from %q (cwd %q): the unresolvable include was downgraded to a hint: %+v", tc.arg, tc.cwd, diags)
			}
			if !hasDiagCode(diags, "include_resolve_failed") {
				t.Fatalf("want include_resolve_failed from %q, got %+v", tc.arg, diags)
			}
			if hasDiagCode(diags, "stage_include_unresolved") {
				t.Fatalf("the loose-file hint must not fire inside a real service tree (%q): %+v", tc.arg, diags)
			}
		})
	}
}

// stageSymlink creates a symlink at linkPath pointing at target.
func stageSymlink(t *testing.T, linkPath, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(target, linkPath); err != nil {
		t.Fatalf("Symlink %s -> %s: %v", linkPath, target, err)
	}
}

// TestStageDiagnostics_IncludeRootedAtServiceRoot — the offline resolve must read
// the file the KEEPER reads, and the securejoin ROOT is the whole of that
// contract. securejoin re-roots an escaping symlink at whatever root it is
// given, so rooting at the scenario's own directory instead of at the service
// root does not merely narrow what resolves: for the same include name it opens
// a DIFFERENT file. The keeper roots at the snapshot root and pre-joins
// `scenario/<name>/` (keeper/internal/scenario/include.go ->
// artifact.readSnapshotFile), so the linter must do the same.
//
// A test that only asserts "resolves / does not resolve" cannot catch the third
// case: both rootings exit 0 there and merely check different bytes. So the
// decoy the wrong rooting lands on is deliberately CLEAN and the file the keeper
// would run carries the defect — the assertion is that the defect is reported,
// which is the only observable that distinguishes the two files.
func TestStageDiagnostics_IncludeRootedAtServiceRoot(t *testing.T) {
	t.Run("symlink elsewhere inside the repo resolves", func(t *testing.T) {
		// A flat include name, so this shape predates the subdirectory grammar
		// entirely: before includes were clamped at all it resolved, and the
		// keeper resolves it today. Clamping at the scenario directory turns it
		// into a hard error on a tree production runs fine.
		root := t.TempDir()
		stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
		stageWrite(t, filepath.Join(root, "shared_bodies", "deploy.yml"), stageSharedBody)
		stageSymlink(t, filepath.Join(root, "scenario", "create", "deploy.yml"),
			filepath.Join("..", "..", "shared_bodies", "deploy.yml"))
		scn := filepath.Join(root, "scenario", "create", "main.yml")
		stageWrite(t, scn, "name: create\ntasks:\n  - include: deploy.yml\n"+stageLocalTask)

		diags := stageDiagnostics(scn, stageParse(t, scn))
		if diag.HasErrors(diags) {
			t.Fatalf("a symlink pointing elsewhere INSIDE the service root must resolve, as it does for the keeper: %+v", diags)
		}
	})

	t.Run("absolute symlink re-roots at the service root", func(t *testing.T) {
		root := t.TempDir()
		stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
		// What the keeper runs: `/decoy` re-rooted at the SERVICE root.
		stageWrite(t, filepath.Join(root, "decoy", "body.yml"),
			stageSharedBody+"\n- include: nowhere.yml\n")
		// What a scenario-directory rooting would read instead — clean, so the
		// wrong file betrays itself by the ABSENCE of the diagnostic.
		stageWrite(t, filepath.Join(root, "scenario", "create", "decoy", "body.yml"), stageSharedBody)
		stageSymlink(t, filepath.Join(root, "scenario", "create", "link"), "/decoy")
		scn := filepath.Join(root, "scenario", "create", "main.yml")
		stageWrite(t, scn, "name: create\ntasks:\n  - include: link/body.yml\n"+stageLocalTask)

		diags := stageDiagnostics(scn, stageParse(t, scn))
		if !hasDiagCode(diags, "include_resolve_failed") {
			t.Fatalf("the linter read the decoy under the scenario directory instead of the file the keeper resolves (%s/decoy/body.yml): %+v", root, diags)
		}
	})

	t.Run("the service tier is rooted at the service root too", func(t *testing.T) {
		// The SECOND tier needs its own case. Both tiers are re-rooted by one
		// helper, so a mutation of that helper reddens the local-tier subtests and
		// looks fully covered — but reverting the service tier ALONE stays green
		// across the whole suite, which is to say the exact defect this test
		// exists for can come back on the tier the ticket was written for: the
		// shared bodies of a scenario family live at the service level.
		root := t.TempDir()
		stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
		// The keeper's file: `/decoy` re-rooted at the SERVICE root.
		stageWrite(t, filepath.Join(root, "decoy", "body.yml"),
			stageSharedBody+"\n- include: nowhere.yml\n")
		// Where a `scenario/`-rooted resolve would land instead — clean, so the
		// wrong file betrays itself by the absence of the diagnostic.
		stageWrite(t, filepath.Join(root, "scenario", "decoy", "body.yml"), stageSharedBody)
		stageSymlink(t, filepath.Join(root, "scenario", "link"), "/decoy")
		// No local match, so resolution must fall through to the service tier.
		scn := filepath.Join(root, "scenario", "create", "main.yml")
		stageWrite(t, scn, "name: create\ntasks:\n  - include: link/body.yml\n"+stageLocalTask)

		diags := stageDiagnostics(scn, stageParse(t, scn))
		if !hasDiagCode(diags, "include_resolve_failed") {
			t.Fatalf("the service tier read %s/scenario/decoy/body.yml instead of the file the keeper resolves (%s/decoy/body.yml): %+v", root, root, diags)
		}
	})

	t.Run("symlink out of the service root is refused", func(t *testing.T) {
		// The clamp is the reason to use securejoin at all: a lexical check never
		// touches the file system and so cannot see this.
		outside := t.TempDir()
		stageWrite(t, filepath.Join(outside, "body.yml"), stageSharedBody)
		root := t.TempDir()
		stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
		stageSymlink(t, filepath.Join(root, "scenario", "create", "body.yml"),
			filepath.Join(outside, "body.yml"))
		scn := filepath.Join(root, "scenario", "create", "main.yml")
		stageWrite(t, scn, "name: create\ntasks:\n  - include: body.yml\n"+stageLocalTask)

		diags := stageDiagnostics(scn, stageParse(t, scn))
		if !hasDiagCode(diags, "include_resolve_failed") {
			t.Fatalf("a symlink pointing OUT of the service root must be refused: %+v", diags)
		}
	})
}

// --- ADR-0084 ordering guards ---
//
// The config-level tests own the predicates themselves. What only the linter can
// prove is the reason both live at stage level: the capture and the task it must be
// ordered against routinely arrive from DIFFERENT files, so the check has to run on
// the EXPANDED plan. A per-file task rule sees one half of the pair and blesses it
// (soul-lint's per-task rules are blind across an `include:` boundary).

// storeAfterUseConsumer — a shared body that configures a live host with a password
// generated in main.yml. The capture that would store it comes after the include: a
// crash in that window leaves the host demanding a password that exists nowhere.
//
// The generator stays in main.yml on purpose: register references are resolved
// per-file at parse time, so a register declared inside an included body is not
// visible to the reference check in main.yml. That asymmetry is the reason the
// ORDERING check has to be a stage-level rule in the first place.
const storeAfterUseConsumer = `- name: configure the host with the generated password
  module: core.exec.run
  changed_when: false
  params:
    cmd: "redis-cli config set requirepass ${ register.gen.stdout }"
`

const storeAfterUseGenerate = `  - name: generate the admin password
    module: core.exec.run
    register: gen
    changed_when: false
    params:
      cmd: "openssl rand -hex 16"
`

const storeAfterUseCapture = `  - name: capture the admin password
    module: core.state.set
    params:
      field: admin_password
      value: "${ register.gen.stdout }"
`

func TestStageDiagnostics_StoreAfterUseAcrossInclude(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "configure.yml"), storeAfterUseConsumer)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n"+storeAfterUseGenerate+
		"  - include: _create/configure.yml\n"+storeAfterUseCapture)

	diags := stageDiagnostics(main, stageParse(t, main))
	d := diagWithCode(diags, config.CodeStateStoreAfterUse)
	if d == nil {
		t.Fatalf("want %s across the include boundary, got %+v", config.CodeStateStoreAfterUse, diags)
	}
	if d.Level != diag.LevelError {
		t.Errorf("level = %v, want Error -- an unstored value on a live host must fail the lint", d.Level)
	}
	if !strings.Contains(d.Message, "configure the host with the generated password") ||
		!strings.Contains(d.Message, "capture the admin password") {
		t.Errorf("the message must name BOTH ends of the pair: %q", d.Message)
	}
}

// TestStageDiagnostics_StoreAfterUseCorrectOrder — ★ REVERSE. The same three tasks
// with the capture pulled AHEAD of the included consumer: generate → store → use, the
// order the ADR prescribes. It must lint clean, or the guard bans the idiom it exists
// to enforce.
func TestStageDiagnostics_StoreAfterUseCorrectOrder(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "configure.yml"), storeAfterUseConsumer)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n"+storeAfterUseGenerate+storeAfterUseCapture+
		"  - include: _create/configure.yml\n")

	diags := stageDiagnostics(main, stageParse(t, main))
	if hasDiagCode(diags, config.CodeStateStoreAfterUse) {
		t.Fatalf("generate -> store -> use must lint clean: %+v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("no errors expected: %+v", diags)
	}
}

// TestStageDiagnostics_StaleStateReadAcrossInclude — the second rule, same boundary:
// the capture is in main.yml, the interpolated read of the field it writes arrives
// from the included body and lands in the SAME Passage, where an interpolated read
// still renders the pre-capture value.
func TestStageDiagnostics_StaleStateReadAcrossInclude(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_create", "announce.yml"),
		`- name: point the replicas at the endpoint
  module: core.exec.run
  changed_when: false
  params:
    cmd: "redis-cli replicaof ${ incarnation.state.endpoint } 6379"
`)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ntasks:\n"+
		`  - name: capture the endpoint
    module: core.state.set
    params:
      field: endpoint
      value: "10.0.0.1"
`+"  - include: _create/announce.yml\n")

	diags := stageDiagnostics(main, stageParse(t, main))
	d := diagWithCode(diags, config.CodeStateStaleRead)
	if d == nil {
		t.Fatalf("want %s across the include boundary, got %+v", config.CodeStateStaleRead, diags)
	}
	if d.Level != diag.LevelError {
		t.Errorf("level = %v, want Error", d.Level)
	}
	if !strings.Contains(d.Message, "endpoint") {
		t.Errorf("the message must name the field: %q", d.Message)
	}
}

// TestStageDiagnostics_WideMatchAcrossInclude — ★ the ADR-057 §d fuse, re-anchored
// by [ADR-0084]. A `remove` with no `match:` demolishes the whole collection; the
// author almost always meant to name one element. It is the one safeguard of the
// removed grammar with no equivalent on the module path — a module manifest can
// require a param, but cannot say "this one is suspicious when it says `true`".
//
// The subject is again the include boundary: the capture arrives from a shared
// body, where the per-file task rules never see it.
func TestStageDiagnostics_WideMatchAcrossInclude(t *testing.T) {
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
	stageWrite(t, filepath.Join(root, "scenario", "_update", "purge.yml"),
		`- name: drop the user
  module: core.state.remove
  params:
    field: redis_users
`)
	main := filepath.Join(root, "scenario", "update", "main.yml")
	stageWrite(t, main, "name: update\ntasks:\n  - include: _update/purge.yml\n")

	diags := stageDiagnostics(main, stageParse(t, main))
	d := diagWithCode(diags, config.CodeStateWideMatch)
	if d == nil {
		t.Fatalf("want %s across the include boundary, got %+v", config.CodeStateWideMatch, diags)
	}
	if d.Level != diag.LevelWarning {
		t.Errorf("level = %v, want Warning -- removing a whole collection is legitimate, just rarely intended", d.Level)
	}
	if !strings.Contains(d.Message, "redis_users") || !strings.Contains(d.Message, "drop the user") {
		t.Errorf("the message must name the field and the task: %q", d.Message)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("a wide match must not fail the lint: %+v", diags)
	}
}

// TestStageDiagnostics_WideMatchConstTrueAndNarrow — the other two halves of the
// fuse in one place: a literal `true` predicate is just as wide as an absent one,
// and a predicate over `elem` is not reported at all. Without the negative half
// the rule could warn on everything and still look correct.
func TestStageDiagnostics_WideMatchConstTrueAndNarrow(t *testing.T) {
	capture := func(match string) string {
		return `  - name: patch the users
    module: core.state.modify
    params:
      field: redis_users
      match: "` + match + `"
      patch:
        acl: "+@read"
`
	}
	for _, tc := range []struct {
		name  string
		match string
		wide  bool
	}{
		{"literal true", "true", true},
		{"wrapped true", "${ true }", true},
		{"over elem", "elem.sid == 'host-a'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			stageWrite(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\n")
			main := filepath.Join(root, "scenario", "update", "main.yml")
			stageWrite(t, main, "name: update\ntasks:\n"+capture(tc.match))

			diags := stageDiagnostics(main, stageParse(t, main))
			if got := hasDiagCode(diags, config.CodeStateWideMatch); got != tc.wide {
				t.Fatalf("wide match warn = %v, want %v for match: %q -- %+v", got, tc.wide, tc.match, diags)
			}
		})
	}
}
