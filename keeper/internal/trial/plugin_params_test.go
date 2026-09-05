package trial

// NIM-790: an L0 case must not go green over a plugin step nobody checked.
//
// soul-trial had no `--modules` at all, loaded every definition with empty
// [config.ValidateOptions], expanded every `include:` with no resolver, and
// filtered both sets of diagnostics to errors before printing. So the
// `plugin_params_unchecked` hint was produced on every plugin step in the corpus,
// discarded, and never seen — and a PASS with nothing beside it is exactly what
// "checked and clean" looks like. That is the fourth tool of this shape after
// NIM-778/779/783, and the one that silence reached last.
//
// The fixture is [deftest], the same bytes and the same manifest `shared/
// definition` and `soul-lint` are tested against: the acceptance criterion of this
// ticket is that the tools AGREE, and three tools in three Go modules cannot be
// run in one test process. What ties them is the input and the expected codes.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/definition"
	"github.com/souls-guild/soul-stack/shared/definition/deftest"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Every runner carries a render attempt's notices onto its Result through
// [renderedCase.carryTo], and the L2 runner is behind `//go:build integration` — it
// does not run in `make check`, so the only thing standing between it and a
// silently zero field is that both runners call the SAME method. This pins the
// method: delete either assignment in it and RunCase's own guards go red with it.
func TestCarryTo_MovesTheNoticesOntoTheResult(t *testing.T) {
	var rc renderedCase
	rc.service, rc.serviceStated = "redis", true
	want := diag.Diagnostic{Level: diag.LevelHint, Code: deftest.Unchecked, File: "main.yml", Line: 7}
	rc.notices.keep([]diag.Diagnostic{want})

	var res Result
	rc.carryTo(&res)

	if len(res.Notices) != 1 || res.Notices[0] != want {
		t.Errorf("Notices = %v, want exactly %v", res.Notices, want)
	}
	if res.Service != "redis" || !res.ServiceStated {
		t.Errorf("Service = %q/%v, want redis/true", res.Service, res.ServiceStated)
	}
}

// writePluginScenarioTree lays out an L0 case whose scenario carries the canonical
// plugin step twice — once inline in main.yml, once behind an `include:`. Those
// are the two wiring paths, and every gap of this class lost exactly one.
func writePluginScenarioTree(t *testing.T, p deftest.Params) string {
	t.Helper()
	caseDir := writeScenarioTree(t, deftest.ScenarioMain("create", "body.yml", p),
		`name: plugin params
assert:
  rendered_tasks:
    - index: 0
      module: `+deftest.Address+`
    - index: 1
      module: `+deftest.Address+`
`)
	writeScenarioSibling(t, caseDir, "body.yml", deftest.TaskList("included step", p))
	return caseDir
}

// bindFixtureManifest is the `--modules` binding of this run, loaded through the
// same [definition.LoadSchemas] the `soul-trial run` flag goes through.
func bindFixtureManifest(t *testing.T) Options {
	t.Helper()
	r, err := definition.LoadSchemas([]string{deftest.Binding(t)})
	if err != nil {
		t.Fatalf("LoadSchemas: %v", err)
	}
	return Options{Modules: r}
}

// noticeCodes is the plugin-params half of a case's notices, which is the part
// comparable with the other tools' verdicts.
func noticeCodes(r Result) []string {
	var out []string
	for _, d := range r.Notices {
		switch d.Code {
		case deftest.Unchecked, deftest.UndeclaredParam, deftest.MissingParam:
			out = append(out, d.Code)
		}
	}
	return out
}

func countNotice(r Result, code string) int {
	n := 0
	for _, c := range noticeCodes(r) {
		if c == code {
			n++
		}
	}
	return n
}

// ★ THE guard. No manifest bound, so the params were checked by nobody — and the
// case says so instead of printing a bare PASS.
//
// Twice, once per wiring path: the inline step comes from the scenario's own parse
// and the included one from the expansion. A fix that reaches only one of them
// passes a test that counts ≥1, which is why this counts exactly 2.
func TestL0_UncheckedPluginParamsReachTheResult(t *testing.T) {
	caseDir := writePluginScenarioTree(t, deftest.Declared)

	results, err := Run(context.Background(), caseDir, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS — an unbound manifest is not the case author's defect: %v", results[0].Failures)
	}
	if got := countNotice(results[0], deftest.Unchecked); got != 2 {
		t.Fatalf("%s ×%d, want 2 (the inline step and the included one); notices: %v",
			deftest.Unchecked, got, results[0].Notices)
	}
	// The one that matters most, because it is the one the harness used to lose:
	// the finding names the file it is about, so the reader is sent to the body
	// rather than to the case.
	var files []string
	for _, d := range results[0].Notices {
		if d.Code == deftest.Unchecked {
			files = append(files, filepath.Base(d.File))
		}
	}
	for _, want := range []string{"main.yml", "body.yml"} {
		if !contains(files, want) {
			t.Errorf("no %s finding for %s; reported at %v", deftest.Unchecked, want, files)
		}
	}
}

// And with the manifest bound the check is REAL: a param the module does not
// declare is an error, and an L0 run refuses the case rather than rendering it.
//
// Refusing the whole run rather than failing the one case is L0's existing
// position on a scenario that does not validate (`trial: scenario ... invalid`),
// not something this ticket introduced — an unrenderable definition has no plan to
// assert against. What NIM-790 changes is that the error is reachable at all.
func TestL0_BoundManifestCatchesUndeclaredParam(t *testing.T) {
	caseDir := writePluginScenarioTree(t, deftest.Undeclared)

	_, err := Run(context.Background(), caseDir, bindFixtureManifest(t))
	if err == nil {
		t.Fatal("a param the manifest does not declare rendered clean")
	}
	if !strings.Contains(err.Error(), deftest.UndeclaredParam) {
		t.Errorf("the run failed for another reason: %v", err)
	}
}

// The required-param half of the same check.
func TestL0_BoundManifestCatchesMissingRequiredParam(t *testing.T) {
	caseDir := writePluginScenarioTree(t, deftest.MissingRequired)

	_, err := Run(context.Background(), caseDir, bindFixtureManifest(t))
	if err == nil {
		t.Fatal("a step missing a required param rendered clean")
	}
	if !strings.Contains(err.Error(), deftest.MissingParam) {
		t.Errorf("the run failed for another reason: %v", err)
	}
}

// Checked and clean is quiet. Without this the two above pass on a harness that
// reports something for every plugin step it sees, which is the same false signal
// as reporting nothing — just in the other direction.
func TestL0_CheckedAndCleanIsQuiet(t *testing.T) {
	caseDir := writePluginScenarioTree(t, deftest.Declared)

	results, err := Run(context.Background(), caseDir, bindFixtureManifest(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS: %v", results[0].Failures)
	}
	if got := noticeCodes(results[0]); len(got) != 0 {
		t.Errorf("a step matching its manifest produced %v", got)
	}
}

// A destiny's own task file is the third place a plugin step arrives from, and it
// is loaded by the resolver rather than by the harness — so it needs the manifests
// threaded through a second path. It goes through the same
// [definition.LoadDestinyTasks] `soul-lint validate-destiny` uses.
func TestL0_DestinyTasksArePluginChecked(t *testing.T) {
	scenario := `name: create
tasks:
  - name: apply the destiny
    apply:
      destiny: acl
      input: {}
`
	caseYML := `name: destiny plugin params
fixtures:
  default_destiny_source: file://destiny-{name}
assert:
  rendered_tasks:
    - index: 0
      module: ` + deftest.Address + `
`
	t.Run("unchecked without a manifest", func(t *testing.T) {
		caseDir := writeApplyDestinyTree(t, "acl", scenario, caseYML,
			"name: acl\n", deftest.TaskList("destiny step", deftest.Declared))

		results, err := Run(context.Background(), caseDir, Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !results[0].Pass {
			t.Fatalf("expected PASS: %v", results[0].Failures)
		}
		if got := countNotice(results[0], deftest.Unchecked); got != 1 {
			t.Fatalf("%s ×%d, want 1 for the destiny's task file; notices: %v",
				deftest.Unchecked, got, results[0].Notices)
		}
	})

	t.Run("checked with one", func(t *testing.T) {
		caseDir := writeApplyDestinyTree(t, "acl", scenario, caseYML,
			"name: acl\n", deftest.TaskList("destiny step", deftest.Undeclared))

		_, err := Run(context.Background(), caseDir, bindFixtureManifest(t))
		if err == nil {
			t.Fatal("an undeclared param in a destiny's tasks/main.yml rendered clean")
		}
		if !strings.Contains(err.Error(), deftest.UndeclaredParam) {
			t.Errorf("the run failed for another reason: %v", err)
		}
	})
}

// The same definition, judged by this harness and by the shared loader directly,
// reaches the same verdict.
//
// This is the cross-tool criterion of NIM-790 as far as one module can carry it:
// `soul-lint` lives in another Go module and cannot be called from here, so what
// is compared is soul-trial against the library both tools now go through, over
// the fixture soul-lint's own test uses. A harness that quietly stopped threading
// the manifests would keep passing every test above that counts findings — it
// would simply report a DIFFERENT set from the one everything else reports.
func TestL0_AgreesWithTheSharedLoader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  deftest.Params
		opts    Options
		modules config.ModuleManifestResolver
	}{
		{"unbound", deftest.Declared, Options{}, nil},
		{"clean", deftest.Declared, bindFixtureManifest(t), bindFixtureManifest(t).Modules},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caseDir := writePluginScenarioTree(t, tc.params)
			results, err := Run(context.Background(), caseDir, tc.opts)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			// The same two files, through the library alone.
			scnPath := filepath.Join(filepath.Dir(filepath.Dir(caseDir)), "main.yml")
			src, rerr := os.ReadFile(scnPath)
			if rerr != nil {
				t.Fatalf("read scenario: %v", rerr)
			}
			m, _, parseDiags, perr := config.LoadScenarioManifestFromBytes(scnPath, src,
				config.ValidateOptions{ModuleManifests: tc.modules})
			if perr != nil {
				t.Fatalf("parse scenario: %v", perr)
			}
			_, expandDiags := definition.ExpandScenario(scnPath, m.Tasks,
				fixtureScenarioIncludeResolver(scnPath), tc.modules)

			// Non-errors only, because that is what the harness's notices channel
			// carries — an error travels as an aborted run instead. Counting both
			// sides the same way is the whole point: a mismatched count would fail
			// this test for the wrong reason and hide a real divergence behind it.
			want := 0
			for _, d := range append(parseDiags, expandDiags...) {
				if d.Level == diag.LevelError {
					continue
				}
				switch d.Code {
				case deftest.Unchecked, deftest.UndeclaredParam, deftest.MissingParam:
					want++
				}
			}
			if got := len(noticeCodes(results[0])); got != want {
				t.Errorf("the harness reports %d plugin-params findings, the shared loader %d: %v",
					got, want, results[0].Notices)
			}
		})
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
