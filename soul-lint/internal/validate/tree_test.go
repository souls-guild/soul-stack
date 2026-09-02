package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const brokenTree = "../../testdata/service-tree-broken"

func runTree(t *testing.T, opts TreeOptions) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunTree(opts, &out, &errOut)
	return code, out.String(), errOut.String()
}

// THE guard of NIM-753. A service broken in the manifest AND in a scenario must
// print BOTH, not the first.
//
// This is the assertion the whole mode exists for. The failure it pins is not
// hypothetical: in the WB redis service a one-line manifest error stopped the
// repository's `set -e` script before it reached a single scenario, and eight
// divergences sat behind it — a dead state_changes: block among them, thirteen
// state fields silently not written — for as long as the manifest stayed red.
//
// It asserts the DIAGNOSTICS, not the exit code. An exit code of 1 is produced by
// a run that stopped at the manifest just as much as by one that finished, so a
// test that checks it alone passes on the very behaviour this replaces.
func TestRunTree_OneBrokenPartDoesNotHideTheOthers(t *testing.T) {
	code, out, errOut := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d (stderr=%q)", code, ExitHasErrors, errOut)
	}
	// One expectation per part, each naming the part's own file: an assertion on
	// the codes alone would pass if one file's findings were attributed to
	// another.
	for _, want := range []struct{ what, file, code string }{
		{"the manifest", "service.yml", "input_type_invalid"},
		{"the first scenario", "scenario/alpha/main.yml", "unknown_register_reference"},
		{"the second scenario's include body", "scenario/beta/capture.yml", "block_on_keeper_invalid"},
	} {
		if !containsDiagFor(out, want.file, want.code) {
			t.Errorf("%s was not reported: no %s at %s\n--- output ---\n%s", want.what, want.code, want.file, out)
		}
	}
}

// The manifest's WARNING survives too. `required: true` next to a `default:` is
// the actual divergence that hid in that service, and a walk that reported only
// the part's errors would drop it while still looking like it worked.
func TestRunTree_KeepsNonFatalDiagnosticsOfABrokenPart(t *testing.T) {
	_, out, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	if !containsDiagFor(out, "service.yml", "input_required_default_conflict") {
		t.Errorf("the manifest's warning was dropped:\n%s", out)
	}
}

// The whole-service mode does not inherit NIM-716 — the neighbouring defect where
// an error out of an included file is downgraded to a `stage_include_unresolved`
// hint with exit 0, saying the include did not resolve when it resolved and was
// read.
//
// Be clear about what this test is and is not. A tree run is inside a service tree
// by construction, so the downgrade never applied here even BEFORE the narrowing —
// meaning this case passes against the old behaviour too, and is not a guard for
// the fix. It guards the STRUCTURAL claim instead: whatever the loose-file rule
// becomes, a tree walk reports an included body error at the body's coordinates.
// The narrowing itself is guarded in stage_test.go
// (TestStageDiagnostics_LooseFileReportsIncludeBodyErrors), where the loose-file
// path is the subject and reverting the condition does turn it red.
func TestRunTree_DoesNotInheritTheIncludeDowngrade(t *testing.T) {
	code, out, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d", code, ExitHasErrors)
	}
	if strings.Contains(out, "stage_include_unresolved") {
		t.Errorf("an error from a locally-resolved include body was downgraded (NIM-716):\n%s", out)
	}
	if !containsDiagFor(out, "scenario/beta/capture.yml", "block_on_keeper_invalid") {
		t.Errorf("the include body's error is not reported at the body's own coordinates:\n%s", out)
	}
}

// A type catalog nobody references is read by nothing — the manifest's
// state_schema and every scenario's input: parse it only to resolve a `$type`,
// and there is none here. So both per-file commands print OK and exit 0 on this
// tree, and the walk is the only thing that finds it.
//
// The per-file halves run in the same test on purpose: the claim is the
// CONTRAST, and asserting the tree's red alone would still pass if the per-file
// commands had quietly started catching it too.
func TestRunTree_ChecksAnUnreferencedTypeCatalog(t *testing.T) {
	const root = "../../testdata/service-tree-bad-types"

	var out, errOut bytes.Buffer
	if code := Run(Options{Path: filepath.Join(root, "service.yml"), Kind: KindService}, &out, &errOut); code != ExitOK {
		t.Fatalf("validate-service on this tree = %d, want ExitOK — the fixture no longer isolates the catalog\n%s", code, out.String())
	}
	out.Reset()
	scenario := filepath.Join(root, "scenario", "create", "main.yml")
	if code := Run(Options{Path: scenario, Kind: KindScenario, ServiceName: "bad-types"}, &out, &errOut); code != ExitOK {
		t.Fatalf("validate-scenario on this tree = %d, want ExitOK — the fixture no longer isolates the catalog\n%s", code, out.String())
	}

	code, treeOut, _ := runTree(t, TreeOptions{Root: root, ServiceName: "bad-types"})
	if code != ExitHasErrors {
		t.Fatalf("the tree walk = %d, want %d — the broken catalog was not read\n%s", code, ExitHasErrors, treeOut)
	}
	if !containsDiagFor(treeOut, "types.yml", "input_type_invalid") {
		t.Errorf("the catalog's own error is missing:\n%s", treeOut)
	}
}

// `scenario/_shared/` holds include bodies, not a scenario — the convention
// `make lint` already follows. Its main.yml is a bare task list, so validating it
// AS a scenario would invent errors about a correct file.
func TestRunTree_SkipsUnderscoreScenarioDirectories(t *testing.T) {
	_, out, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	if strings.Contains(out, "_shared") {
		t.Errorf("a shared-include directory was checked as a scenario:\n%s", out)
	}
}

// Both spellings of the positional address the same tree, so they must produce
// the same report — down to the paths in it, which are built by joining onto the
// root that was derived here.
func TestRunTree_DirAndManifestPathAgree(t *testing.T) {
	_, byDir, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	_, byFile, _ := runTree(t, TreeOptions{Root: filepath.Join(brokenTree, "service.yml"), ServiceName: "broken"})
	if byDir != byFile {
		t.Errorf("the two spellings of the same tree disagree:\n--- <dir> ---\n%s\n--- <dir>/service.yml ---\n%s", byDir, byFile)
	}
}

// Identical input, byte-identical output. Within one process this can only catch
// a non-determinism the process itself carries — map iteration order, most
// plausibly, since a diagnostic set built by ranging a map is exactly how a
// report starts reshuffling between runs. It cannot catch a filesystem that
// returns entries unsorted, because os.ReadDir already sorts; the explicit
// sort.Strings in the walk is there to state that the order is required rather
// than inherited, and is not what this case exercises.
func TestRunTree_OutputIsDeterministic(t *testing.T) {
	_, first, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	for i := 0; i < 3; i++ {
		_, again, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
		if again != first {
			t.Fatalf("run %d differs from run 0:\n--- 0 ---\n%s\n--- %d ---\n%s", i+1, first, i+1, again)
		}
	}
}

// A directory that is not a service tree is a caller error (exit 2), not a tree
// with findings. Reported on stderr and with nothing on stdout: a report about a
// service that is not there would be read as a service with no problems.
func TestRunTree_NotAServiceTree(t *testing.T) {
	code, out, errOut := runTree(t, TreeOptions{Root: t.TempDir()})
	if code != ExitIOFatal {
		t.Fatalf("exit = %d, want %d", code, ExitIOFatal)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errOut, "not a service tree") {
		t.Errorf("stderr = %q, want it to say what is wrong", errOut)
	}
}

// Some other file of the service is refused rather than read as "the tree this
// file is in". Answering a wider question than the one asked is not wrong output,
// but the reader would have to work out from the report which question got
// answered — and a scenario path, the likeliest slip, is refused for the same
// reason.
func TestRunTree_AFileOtherThanTheManifest(t *testing.T) {
	for _, rel := range []string{
		"types.yml",
		filepath.Join("scenario", "alpha", "main.yml"),
	} {
		code, out, errOut := runTree(t, TreeOptions{Root: filepath.Join(brokenTree, rel)})
		if code != ExitIOFatal {
			t.Errorf("%s → %d, want %d; stdout = %q", rel, code, ExitIOFatal, out)
		}
		if out != "" {
			t.Errorf("%s printed %q, want nothing", rel, out)
		}
		if errOut == "" {
			t.Errorf("%s said nothing on stderr", rel)
		}
	}
}

// JSON mode is the same JSON-Lines stream the per-file commands emit — one
// object per diagnostic and nothing else, so the `OK:` lines of human mode
// cannot leak into a machine consumer's input.
func TestRunTree_JSONIsOneObjectPerDiagnostic(t *testing.T) {
	code, out, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken", JSON: true})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d", code, ExitHasErrors)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	files := map[string]bool{}
	for _, line := range lines {
		var d diag.Diagnostic
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %q is not a diagnostic: %v", line, err)
		}
		files[filepath.Base(d.File)] = true
	}
	// The same three parts as the human-mode guard: JSON is a second rendering of
	// one walk, not a second walk with its own reach.
	for _, want := range []string{"service.yml", "main.yml", "capture.yml"} {
		if !files[want] {
			t.Errorf("no diagnostic from %s in the JSON stream:\n%s", want, out)
		}
	}
}

// A populated migrations/ is CHECKED, not reported as unreadable. NIM-736 gave the
// linter a reading of the ladder, so the `migrations_unchecked` hint this test used
// to assert is retired along with the gap it named.
//
// The broken fixture carries the pre-NIM-735 flat step (`001_to_002.yml`), so the
// walk has to name that layout rather than shrug at the directory — which is the
// stronger claim: the part is not merely opened, it is understood.
func TestRunTree_MigrationsAreChecked(t *testing.T) {
	_, out, _ := runTree(t, TreeOptions{Root: brokenTree, ServiceName: "broken"})
	if !strings.Contains(out, "migration_layout_retired") {
		t.Errorf("the retired flat ladder was not named:\n%s", out)
	}
	if strings.Contains(out, "migrations_unchecked") {
		t.Errorf("the retired unchecked-hint is still raised:\n%s", out)
	}
	// A part with an error never prints `OK:` for itself.
	if strings.Contains(out, "OK: "+filepath.Join(brokenTree, "migrations")) {
		t.Errorf("a part with an error printed OK:\n%s", out)
	}
}

// A ladder with nothing wrong with it IS reported clean — the counterweight to the
// case above. Without it, a check that errored on every populated migrations/ would
// pass that test.
func TestRunTree_CleanLadderIsClean(t *testing.T) {
	root := writeMinimalTree(t)
	step := filepath.Join(root, "migrations", "002_widen_users")
	if err := os.MkdirAll(step, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(step, "main.yml"), []byte("transform: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitOK, out)
	}
	if !strings.Contains(out, "OK: "+filepath.Join(root, "migrations")) {
		t.Errorf("a clean ladder did not report itself checked:\n%s", out)
	}
}

// An absent or empty migrations/ says nothing: an empty ladder is a complete
// statement (the service is at state-schema version 1), not a gap.
func TestRunTree_EmptyMigrationsIsSilent(t *testing.T) {
	root := writeMinimalTree(t)
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitOK, out)
	}
	if strings.Contains(out, "migrations") {
		t.Errorf("an empty ladder produced a part at all:\n%s", out)
	}
}

// A service with no scenario parses, registers and can never be run. A warning,
// so it does not fail a lint — but said out loud, because the alternative is a
// walk that reports one part and looks complete.
func TestRunTree_NoScenariosIsAWarning(t *testing.T) {
	root := writeMinimalTree(t)
	if err := os.RemoveAll(filepath.Join(root, "scenario")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (a warning must not fail the lint):\n%s", code, ExitOK, out)
	}
	if !strings.Contains(out, CodeServiceTreeNoScenarios) {
		t.Errorf("a service with no scenario was not reported:\n%s", out)
	}
	// And it takes no `OK:` line. Nothing was checked, and the directory the part
	// is named after does not even exist — `OK: <root>/scenario` under a warning
	// saying there are no scenarios is the optimistic silence this mode removes.
	// Asserting only the code and the exit is what let that slip through once.
	if strings.Contains(out, "OK: "+filepath.Join(root, "scenario")) {
		t.Errorf("a part nobody checked printed OK:\n%s", out)
	}
}

// The `OK:` lines are a claim about WHICH parts ran, so the set of them is worth
// asserting whole rather than one absence at a time — a part that silently stops
// being walked disappears from this list and from nothing else.
func TestRunTree_OKLinesNameExactlyTheCheckedParts(t *testing.T) {
	root := writeMinimalTree(t)
	writeFile(t, filepath.Join(root, "types.yml"), "types:\n  AclUser:\n    type: string\n")
	writeFile(t, filepath.Join(root, "upgrade", "to_v2", "main.yml"),
		"name: to_v2\nfrom: ['1']\ndescription: Fine\n\ntasks:\n  - name: Write it\n"+
			"    module: core.file.present\n    params:\n      path: /tmp/x\n      content: hello\n")
	writeFile(t, filepath.Join(root, "migrations", "002_widen_users", "main.yml"),
		"transform:\n  - set:\n      path: x\n      value: y\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitOK, out)
	}
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "OK: "); ok {
			got = append(got, strings.TrimPrefix(rest, root+string(filepath.Separator)))
		}
	}
	want := []string{
		"service.yml",
		"types.yml",
		filepath.Join("scenario", "create", "main.yml"),
		filepath.Join("upgrade", "to_v2", "main.yml"),
		// migrations/ joined this list with NIM-736: the ladder is a checked part
		// now, so it earns an `OK:` of its own instead of a "discovered" hint. It
		// sits last because the walk reports it last.
		"migrations",
	}
	if len(got) != len(want) {
		t.Fatalf("OK: lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("OK: line %d = %q, want %q (full set %v)", i, got[i], want[i], got)
		}
	}
}

// The walk drives include resolution too, and an upgrade scenario resolves from
// `scenario/<slug>/` and `scenario/` — so a body beside it is a red tree, not a
// quiet one. Covered at stage level in stage_test.go; this pins that the walk
// reaches that path at all rather than only the per-file command.
func TestRunTree_UpgradeIncludeIsResolvedLikeTheEngine(t *testing.T) {
	root := writeMinimalTree(t)
	writeFile(t, filepath.Join(root, "upgrade", "to_v2", "install.yml"),
		"- name: shared step\n  module: core.service.restarted\n  params:\n    name: redis\n")
	writeFile(t, filepath.Join(root, "upgrade", "to_v2", "main.yml"),
		"name: to_v2\nfrom: ['1']\ndescription: The body sits beside this file\n\ntasks:\n  - include: install.yml\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d - a body the run cannot reach was blessed:\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("upgrade", "to_v2", "main.yml"), "include_resolve_failed") {
		t.Errorf("no resolve failure on the upgrade scenario:\n%s", out)
	}
	// The author is told where the engine WILL look, not just that it failed.
	if !strings.Contains(out, "NOT from upgrade/<slug>/") {
		t.Errorf("the channel asymmetry was not explained to the author:\n%s", out)
	}
	// The healthy scenario beside it is still reported.
	if !strings.Contains(out, "OK: "+filepath.Join(root, "scenario", "create", "main.yml")) {
		t.Errorf("the other channel was not walked:\n%s", out)
	}
}

// `scenario/<name>/` IS how a scenario is addressed, so one that answers to a
// name and has no entry point is a defect, not something to skip quietly.
func TestRunTree_ScenarioDirWithoutEntryPoint(t *testing.T) {
	root := writeMinimalTree(t)
	if err := os.Mkdir(filepath.Join(root, "scenario", "orphan"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("orphan", "main.yml"), "io_error") {
		t.Errorf("a scenario directory with no main.yml was skipped in silence:\n%s", out)
	}
	// And the healthy scenario beside it is still reported — the same rule as for
	// every other part.
	if !strings.Contains(out, filepath.Join("scenario", "create", "main.yml")) {
		t.Errorf("the sibling scenario was not reached:\n%s", out)
	}
}

// A `--modules` binding that does not resolve is fatal for the whole run, and it
// is fatal ONCE. Reporting it per scenario would print the same usage error as
// many times as the tree has scenarios and bury it among the service's findings.
func TestRunTree_BrokenModuleBindingIsFatal(t *testing.T) {
	code, out, errOut := runTree(t, TreeOptions{
		Root:    brokenTree,
		Modules: []string{"redis=/nonexistent/schema.json"},
	})
	if code != ExitIOFatal {
		t.Fatalf("exit = %d, want %d", code, ExitIOFatal)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing — no part was checked", out)
	}
	if strings.Count(errOut, "soul-lint:") != 1 {
		t.Errorf("the binding error was reported %d times, want once:\n%s", strings.Count(errOut, "soul-lint:"), errOut)
	}
}

// A part with errors NEVER takes an `OK:` line, even when every one of its
// findings was already printed by an earlier part that read the same file.
//
// This is the regression that a de-duplicating walk introduced and this test
// would have caught: the catalog is parsed by the manifest's `$type` resolve and
// by its own part, both with the same path and the same bytes, so the two produce
// byte-identical diagnostics. Collapsing them emptied the catalog part, and the
// `OK:` decision — read off what SURVIVED — then printed `OK: types.yml` two
// lines under that file's own errors. A green line on a red part is the exact
// failure this mode exists to abolish.
func TestRunTree_ABrokenPartNeverPrintsOK(t *testing.T) {
	root := writeMinimalTree(t)
	// The manifest reaches the catalog through a $type, so the catalog is read
	// twice over: once resolving state_schema, once as a part of its own.
	writeFile(t, filepath.Join(root, "service.yml"),
		"description: Minimal\n\nstate_schema:\n  acl_users:\n    type: array\n    items:\n      $type: AclUser\n")
	writeFile(t, filepath.Join(root, "types.yml"),
		"types:\n  AclUser:\n    type: objct\n    properties:\n      name:\n        type: string\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, "types.yml", "input_type_invalid") {
		t.Fatalf("the catalog's error is missing entirely:\n%s", out)
	}
	if strings.Contains(out, "OK: "+filepath.Join(root, "types.yml")) {
		t.Errorf("a part with errors was declared OK:\n%s", out)
	}
}

// Both scenario auto-discovery channels are walked. `upgrade/<slug>/main.yml` is
// the second one (ADR-0068 §3, the keeper's ListUpgrades beside ListScenarios) —
// same file form, same rules — so a walk that covered only `scenario/` would
// print a clean tree having never opened the other half of it.
func TestRunTree_WalksTheUpgradeChannel(t *testing.T) {
	root := writeMinimalTree(t)
	writeFile(t, filepath.Join(root, "upgrade", "to_v2", "main.yml"),
		"name: to_v2\nfrom: ['1']\ndescription: Broken on purpose\n\ntasks:\n"+
			"  - name: Restart where the probe said\n    module: core.service.restarted\n"+
			"    where: register.absent.stdout == 'x'\n    params:\n      name: redis\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d - the upgrade channel was not walked:\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("upgrade", "to_v2", "main.yml"), "unknown_register_reference") {
		t.Errorf("no diagnostic from the upgrade scenario:\n%s", out)
	}
}

// A service with neither channel populated is the "no scenarios" case, and one
// warning covers both - not one per empty directory.
func TestRunTree_NoScenariosCountsBothChannels(t *testing.T) {
	root := writeMinimalTree(t)
	if err := os.RemoveAll(filepath.Join(root, "scenario")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "upgrade"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if n := strings.Count(out, CodeServiceTreeNoScenarios); n != 1 {
		t.Errorf("reported %d times, want exactly 1:\n%s", n, out)
	}
	// And one scenario in EITHER channel is enough to silence it.
	writeFile(t, filepath.Join(root, "upgrade", "to_v2", "main.yml"),
		"name: to_v2\nfrom: ['1']\ndescription: Fine\n\ntasks:\n  - name: Write it\n"+
			"    module: core.file.present\n    params:\n      path: /tmp/x\n      content: hello\n")
	if _, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"}); strings.Contains(out, CodeServiceTreeNoScenarios) {
		t.Errorf("an upgrade scenario did not count as a scenario:\n%s", out)
	}
}

// A shared-include directory is recognised by the rule's OWNER
// (config.IsSharedDirName), in either channel — not by a copy of it here.
func TestRunTree_SharedDirRuleComesFromItsOwner(t *testing.T) {
	root := writeMinimalTree(t)
	for _, channel := range []string{"scenario", "upgrade"} {
		// A bare task list: valid as an include body, a pile of errors as a scenario.
		writeFile(t, filepath.Join(root, channel, "_shared", "main.yml"),
			"- name: A shared body\n  module: core.file.present\n  params:\n    path: /tmp/s\n    content: s\n")
	}
	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "minimal"})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d - a shared directory was checked as a scenario:\n%s", code, ExitOK, out)
	}
	if strings.Contains(out, "_shared") {
		t.Errorf("a shared-include directory reached the report:\n%s", out)
	}
}

// A panic inside one part's checks is a linter bug, and the walk must survive it:
// it becomes an ERROR naming the file, the run is red, and every other part is
// still reported. Exercised on the helper, because provoking a real panic in the
// parser would mean shipping a document that crashes it.
func TestSafeDiags_TurnsAPanicIntoADiagnostic(t *testing.T) {
	diags := safeDiags("scenario/create/main.yml", func() []diag.Diagnostic {
		panic("index out of range [3] with length 2")
	})
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Level != diag.LevelError {
		t.Errorf("level = %q, want error - a crash must not pass as a hint", diags[0].Level)
	}
	if diags[0].Code != CodeLintInternalPanic {
		t.Errorf("code = %q, want %q", diags[0].Code, CodeLintInternalPanic)
	}
	if !strings.Contains(diags[0].Message, "index out of range") {
		t.Errorf("the panic value was lost: %q", diags[0].Message)
	}
	if diags[0].File != "scenario/create/main.yml" {
		t.Errorf("file = %q, want the file that provoked it", diags[0].File)
	}
	// And the ordinary path is untouched.
	want := []diag.Diagnostic{{Level: diag.LevelHint, Code: "passage_plan"}}
	if got := safeDiags("x.yml", func() []diag.Diagnostic { return want }); len(got) != 1 || got[0].Code != "passage_plan" {
		t.Errorf("safeDiags altered a clean result: %+v", got)
	}
}

// Every Kind must have a case in diagnose. The tree walk drops diagnose's "I do
// not know this Kind" bool — deliberately, since it reports a defect of the
// linter and there is no diagnostic code that would mean anything to an author —
// so this is what keeps a Kind added without a case from silently producing a
// part with no findings.
//
// The list is written out rather than ranged over an iota bound: adding a Kind
// and forgetting it here fails on the same read as adding it and forgetting it in
// diagnose, which is the point.
func TestDiagnose_HandlesEveryKind(t *testing.T) {
	for _, k := range []Kind{KindConfig, KindDestiny, KindService, KindScenario, KindManifest} {
		if _, ok := diagnose(Options{Path: "x.yml", Kind: k}, []byte("{}\n"), nil); !ok {
			t.Errorf("kind %d has no case in diagnose", k)
		}
	}
	// And the sentinel below the last one is still unknown, so the check above is
	// not vacuously true.
	if _, ok := diagnose(Options{Path: "x.yml", Kind: KindManifest + 1}, []byte("{}\n"), nil); ok {
		t.Error("an out-of-range Kind was accepted — either a Kind was added without extending this test, or diagnose lost its default case")
	}
}

// The recover is wired into the type-catalog part, not merely available to it.
//
// This part is the one whose checks are not reached through diagnose, so it is the
// one place the wrapper can be forgotten while every other part stays protected —
// and it WAS forgotten in the first cut of this file, which
// TestSafeDiags_TurnsAPanicIntoADiagnostic passed straight through. A test of a
// safety net has to be a test of where it is hung.
func TestTypeCatalogPart_PanicIsCaught(t *testing.T) {
	root := writeMinimalTree(t)
	writeFile(t, filepath.Join(root, "types.yml"), "types:\n  AclUser:\n    type: string\n")

	part, ok := typeCatalogPartWith(root, func(string, []byte) (config.TypeCatalog, []diag.Diagnostic) {
		panic("invalid memory address or nil pointer dereference")
	})
	if !ok {
		t.Fatal("the catalog part was not produced at all")
	}
	if len(part.diags) != 1 || part.diags[0].Code != CodeLintInternalPanic {
		t.Fatalf("the panic escaped the part: %+v", part.diags)
	}
	if part.diags[0].Level != diag.LevelError {
		t.Errorf("level = %q, want error", part.diags[0].Level)
	}
}

// writeMinimalTree builds the smallest valid service tree — a manifest and one
// correct scenario — in a temp directory, for the cases that are about the WALK
// rather than about any document's content.
func writeMinimalTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "service.yml"),
		"description: Minimal\n\nstate_schema:\n  greeting_file:\n    type: string\n")
	writeFile(t, filepath.Join(root, "scenario", "create", "main.yml"),
		"name: create\ndescription: Minimal\n\ntasks:\n  - name: Write it\n    module: core.file.present\n    params:\n      path: /tmp/x\n      content: hello\n")
	return root
}

// writeFile writes body at an absolute path, creating the parent directories.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// containsDiagFor reports whether the human-mode report holds a diagnostic with
// this code whose FILE path ends in `file`. Matching the pair rather than the
// code alone is what makes the guard above meaningful: every code in it would be
// present in the output of a run that attributed all of them to one file.
//
// The path is matched against the location PREFIX only — `<file>[:line[:col]]`,
// everything before the ` <level>: ` that starts the human format's tail. The
// message must not be allowed to satisfy it: an io_error quotes the path it
// failed on (`stat <path>: no such file`), so a whole-line Contains would accept
// a diagnostic whose File is one document and whose text merely names another.
func containsDiagFor(out, file, code string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "["+code+"]") {
			continue
		}
		// writeHumanDiag emits `<file>:[<line>:[<col>:]] <level>: [<code>] <msg>`,
		// so the location is everything up to the first space.
		loc, _, ok := strings.Cut(line, " ")
		if ok && strings.HasSuffix(trimLineCol(loc), file) {
			return true
		}
	}
	return false
}

// trimLineCol strips the trailing colon and up to two `:<number>` position
// segments from a human-mode location prefix, leaving the file path.
func trimLineCol(loc string) string {
	loc = strings.TrimSuffix(loc, ":")
	for i := 0; i < 2; i++ {
		idx := strings.LastIndexByte(loc, ':')
		if idx < 0 {
			break
		}
		if _, err := strconv.Atoi(loc[idx+1:]); err != nil {
			break
		}
		loc = loc[:idx]
	}
	return loc
}
