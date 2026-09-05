package definition_test

// The rule this package exists to hold, pinned on both entry points: a check that
// could not run SAYS SO, a check that ran reports what it found, and a check that
// ran clean is quiet. Three outcomes, never two.
//
// Silence is the one that has actually shipped a defect (NIM-778), and it is the
// one no test catches by accident: an implementation that reports nothing passes
// every assertion of the form "the output does not contain unknown_param".

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/definition"
	"github.com/souls-guild/soul-stack/shared/definition/deftest"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	scenarioMain = "scenario/create/main.yml"
	scenarioBody = "body.yml"
	destinyMain  = "tasks/main.yml"
	destinyBody  = "body.yml"
)

// mapResolver serves include bodies out of a map, with the map key as the display
// path — the same shape every real resolver produces, minus a file system.
func mapResolver(files map[string]string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		body, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("no such body %q", name)
		}
		return []byte(body), name, nil
	}
}

// loadScenario is what a tool does: parse the entry file with the manifests bound,
// then expand through this package. Both halves are returned as one verdict,
// because the point of the fixture is that a tool which wires only one of them
// loses a finding.
func loadScenario(t *testing.T, p deftest.Params, modules config.ModuleManifestResolver) []diag.Diagnostic {
	t.Helper()
	src := []byte(deftest.ScenarioMain("create", scenarioBody, p))
	m, _, diags, err := config.LoadScenarioManifestFromBytes(scenarioMain, src, config.ValidateOptions{ModuleManifests: modules})
	if err != nil {
		t.Fatalf("parsing the scenario entry point: %v", err)
	}
	if m == nil {
		t.Fatalf("the scenario fixture does not parse: %v", codes(diags))
	}
	_, expandDiags := definition.ExpandScenario(scenarioMain, m.Tasks,
		mapResolver(map[string]string{scenarioBody: deftest.TaskList("included step", p)}), modules)
	return append(diags, expandDiags...)
}

// loadDestiny is the same for a destiny, where this package owns the parse too.
func loadDestiny(t *testing.T, p deftest.Params, modules config.ModuleManifestResolver) []diag.Diagnostic {
	t.Helper()
	src := []byte(deftest.DestinyTasksMain(destinyBody, p))
	_, diags := definition.LoadDestinyTasks(destinyMain, src,
		mapResolver(map[string]string{destinyBody: deftest.TaskList("included step", p)}), modules)
	return diags
}

// bound is the resolver over the fixture manifest.
func bound(t *testing.T) config.ModuleManifestResolver {
	t.Helper()
	r, err := definition.LoadSchemas([]string{deftest.Binding(t)})
	if err != nil {
		t.Fatalf("LoadSchemas: %v", err)
	}
	if r == nil {
		t.Fatal("LoadSchemas returned no resolver for a valid binding")
	}
	return r
}

func codes(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	sort.Strings(out)
	return out
}

func count(diags []diag.Diagnostic, code string) int {
	n := 0
	for _, d := range diags {
		if d.Code == code {
			n++
		}
	}
	return n
}

// ★ THE guard. Nobody bound a manifest, so the params genuinely cannot be checked
// — and that is a thing to SAY, once per plugin step, at both wiring paths.
//
// Two, not one: the inline step comes from the entry file's own parse and the
// included step from the expansion. Every historical gap in this area lost exactly
// one of the two and left the other looking correct.
func TestUnresolvedManifestIsReportedNotSilent(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		diags []diag.Diagnostic
	}{
		{"scenario", loadScenario(t, deftest.Declared, nil)},
		{"destiny", loadDestiny(t, deftest.Declared, nil)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if got := count(tc.diags, deftest.Unchecked); got != 2 {
				t.Fatalf("%s ×%d, want 2 (the inline step and the included one): %v",
					deftest.Unchecked, got, codes(tc.diags))
			}
			// A hint, never an error: the author of a definition usually cannot
			// produce somebody else's plugin's manifest, so failing them for its
			// absence would punish the wrong person.
			for _, d := range tc.diags {
				if d.Code == deftest.Unchecked && d.Level != diag.LevelHint {
					t.Errorf("%s is %q; it must not fail a run — nothing in the definition is wrong",
						deftest.Unchecked, d.Level)
				}
			}
			if diag.HasErrors(tc.diags) {
				t.Errorf("an unbound manifest produced an error: %v", codes(tc.diags))
			}
		})
	}
}

// With the manifest bound the check is REAL, at both paths, and the hint is gone —
// a run that reports both has not checked anything, it has reported unconditionally.
func TestBoundManifestChecksBothPaths(t *testing.T) {
	modules := bound(t)
	for _, tc := range []struct {
		kind  string
		diags []diag.Diagnostic
	}{
		{"scenario", loadScenario(t, deftest.Undeclared, modules)},
		{"destiny", loadDestiny(t, deftest.Undeclared, modules)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if got := count(tc.diags, deftest.UndeclaredParam); got != 2 {
				t.Fatalf("%s ×%d, want 2: %v", deftest.UndeclaredParam, got, codes(tc.diags))
			}
			if got := count(tc.diags, deftest.Unchecked); got != 0 {
				t.Errorf("reported %d steps as unchecked while checking them: %v",
					got, codes(tc.diags))
			}
			if !diag.HasErrors(tc.diags) {
				t.Errorf("a param the manifest does not declare is an error, got: %v", codes(tc.diags))
			}
		})
	}
}

// The required-param half of the same check.
func TestBoundManifestCatchesMissingRequiredParam(t *testing.T) {
	modules := bound(t)
	for _, tc := range []struct {
		kind  string
		diags []diag.Diagnostic
	}{
		{"scenario", loadScenario(t, deftest.MissingRequired, modules)},
		{"destiny", loadDestiny(t, deftest.MissingRequired, modules)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if got := count(tc.diags, deftest.MissingParam); got != 2 {
				t.Errorf("%s ×%d, want 2: %v", deftest.MissingParam, got, codes(tc.diags))
			}
		})
	}
}

// Checked and clean is quiet. Without this the two tests above pass on an
// implementation that emits a finding for every plugin step it sees.
func TestCheckedAndCleanIsQuiet(t *testing.T) {
	modules := bound(t)
	for _, tc := range []struct {
		kind  string
		diags []diag.Diagnostic
	}{
		{"scenario", loadScenario(t, deftest.Declared, modules)},
		{"destiny", loadDestiny(t, deftest.Declared, modules)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, c := range []string{deftest.Unchecked, deftest.UndeclaredParam, deftest.MissingParam} {
				if count(tc.diags, c) != 0 {
					t.Errorf("a step matching its manifest produced %s: %v", c, codes(tc.diags))
				}
			}
		})
	}
}

// Both tools resolve the SAME definition to the SAME verdict. A scenario and a
// destiny are different documents, but the plugin-params half of their verdicts is
// the same check over the same step, and a tool that wires only one path would
// break this equality rather than merely reporting less.
func TestScenarioAndDestinyAgree(t *testing.T) {
	modules := bound(t)
	for _, tc := range []struct {
		name    string
		params  deftest.Params
		modules config.ModuleManifestResolver
	}{
		{"unbound", deftest.Declared, nil},
		{"undeclared param", deftest.Undeclared, modules},
		{"missing required", deftest.MissingRequired, modules},
		{"clean", deftest.Declared, modules},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scn := pluginCodes(loadScenario(t, tc.params, tc.modules))
			dst := pluginCodes(loadDestiny(t, tc.params, tc.modules))
			if strings.Join(scn, ",") != strings.Join(dst, ",") {
				t.Errorf("the two document kinds disagree on one input:\n  scenario: %v\n  destiny:  %v", scn, dst)
			}
		})
	}
}

// pluginCodes narrows a verdict to the plugin-params findings — the part that is
// the same check on both sides. A scenario carries a manifest and a destiny's task
// file does not, so their full verdicts legitimately differ.
func pluginCodes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		switch d.Code {
		case deftest.Unchecked, deftest.UndeclaredParam, deftest.MissingParam:
			out = append(out, d.Code)
		}
	}
	sort.Strings(out)
	return out
}

// A finding out of an included body is reported at the BODY's own file, not at the
// entry point that spliced it: an author sent to main.yml for a param written two
// files away has been told the wrong place to look (NIM-716).
func TestIncludedFindingKeepsTheBodysFile(t *testing.T) {
	modules := bound(t)
	for _, tc := range []struct {
		kind  string
		body  string
		diags []diag.Diagnostic
	}{
		{"scenario", scenarioBody, loadScenario(t, deftest.Undeclared, modules)},
		{"destiny", destinyBody, loadDestiny(t, deftest.Undeclared, modules)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var files []string
			for _, d := range tc.diags {
				if d.Code == deftest.UndeclaredParam {
					files = append(files, d.File)
				}
			}
			var found bool
			for _, f := range files {
				if filepath.Base(f) == tc.body {
					found = true
				}
			}
			if !found {
				t.Errorf("no finding at the included body %q; reported at %v", tc.body, files)
			}
		})
	}
}

// Every diagnostic gets a file, including the cross-file ones the expander
// produces after AST positions are gone. A finding with no file is one a reader
// cannot open.
func TestEveryFindingNamesAFile(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		diags []diag.Diagnostic
	}{
		{"scenario", loadScenario(t, deftest.Declared, nil)},
		{"destiny", loadDestiny(t, deftest.Declared, nil)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, d := range tc.diags {
				if d.File == "" {
					t.Errorf("[%s] %s carries no file", d.Code, d.Message)
				}
			}
		})
	}
}

// A parse-fatal task file has no list to expand, and an include tree derived from
// one would describe a plan that never existed. The parse's verdict still comes
// back — the caller must not be handed silence here either.
func TestDestinyParseFatalStillReports(t *testing.T) {
	_, diags := definition.LoadDestinyTasks(destinyMain, []byte("this: is not a task list\n"),
		mapResolver(nil), nil)
	if len(diags) == 0 {
		t.Fatal("a task file that does not parse produced no diagnostics")
	}
	if !diag.HasErrors(diags) {
		t.Errorf("a task file that does not parse is an error: %v", codes(diags))
	}
}

// An unresolvable include is the body's failure, not a reason to drop the rest of
// the verdict: the strictest invocation must not report the least.
func TestUnresolvableIncludeDoesNotSwallowTheEntryFile(t *testing.T) {
	src := []byte(deftest.DestinyTasksMain("gone.yml", deftest.Declared))
	_, diags := definition.LoadDestinyTasks(destinyMain, src, mapResolver(nil), nil)

	if count(diags, deftest.Unchecked) != 1 {
		t.Errorf("the entry file's own step lost its %s when the include failed: %v",
			deftest.Unchecked, codes(diags))
	}
	if !diag.HasErrors(diags) {
		t.Errorf("an include that does not resolve is an error: %v", codes(diags))
	}
}
