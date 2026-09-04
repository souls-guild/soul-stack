package validate

// NIM-779: a plugin step written in an `include:`d body is checked, or says it
// was not — the linter must not go quiet about it.
//
// The plugin-params post-pass is per-document ([config.ValidateOptions.
// ModuleManifests] reaches a parse entry point, not `UnmarshalYAML`), so a step
// living in an included file is reached only through the expansion. That path
// carried neither the resolver nor the body's non-fatal findings, so the linter
// said nothing at all about the step: not `unknown_param`, not
// `plugin_params_unchecked`. Silence is the worst of the three outcomes — it is
// indistinguishable from "checked and clean", which is how NIM-778 (`tls: "true"`
// as a string, plaintext with a password) survived a green lint.
//
// The subject here is the WIRING, not the walk ([config] covers that): both
// commands must reach it, since `validate-service-tree` (NIM-753) is what a
// service repository actually runs.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// A keeper-side cloud module with one required param, shaped like the live
// `soul-cloud-wb` document this ticket came off: `wb-cloud.vm.created`.
const vmSchemaJSON = `{"kind":"soul_module","protocol_version":1,` +
	`"modules":[{"name":"vm","side":"keeper","description":"cloud VMs","states":{"created":{` +
	`"description":"the batch exists","input":{` +
	`"count":{"type":"int","required":true},"name":{"type":"string"}}}}}]}`

// includeTree writes a service whose only plugin step sits in a service-level
// include, the layout of the WB redis service: `scenario/create/main.yml`
// includes `provision.yml`, resolved one level up at `scenario/`. params is
// spliced into that step. It returns the tree root.
func includeTree(t *testing.T, params string) string {
	t.Helper()
	root := t.TempDir()
	stageWrite(t, filepath.Join(root, "service.yml"),
		"description: A service whose plugin step lives in an include\n\nstate_schema:\n  greeting:\n    type: string\n")
	stageWrite(t, filepath.Join(root, "scenario", "provision.yml"),
		"- name: provision the batch\n  on: keeper\n  module: wb-cloud.vm.created\n  params:\n"+params)
	stageWrite(t, filepath.Join(root, "scenario", "create", "main.yml"),
		"name: create\ndescription: provisions through an include\n\ntasks:\n  - include: provision.yml\n")
	return root
}

// goodParams matches the schema; bogusParams adds a param the module does not
// declare — the mutation NIM-763 would have caught and did not.
const (
	goodParams  = "    count: 3\n    name: redis\n"
	bogusParams = "    count: 3\n    bogus_param: \"x\"\n"
)

// runScenario is `soul-lint validate-scenario` over the tree's create scenario.
func runScenario(t *testing.T, root string, modules []string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{
		Path:        filepath.Join(root, "scenario", "create", "main.yml"),
		Kind:        KindScenario,
		Modules:     modules,
		ServiceName: "redis",
	}, &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("soul-lint failed to run: %s", errOut.String())
	}
	return code, out.String()
}

// bindVM writes the module document and returns the `--modules` binding for it.
func bindVM(t *testing.T) []string {
	t.Helper()
	return []string{"wb-cloud=" + writeSchemaFile(t, t.TempDir(), "soul-cloud-wb", vmSchemaJSON)}
}

// THE guard. No manifest bound, so the params genuinely cannot be checked — and
// that is a thing to SAY. A run that prints nothing here has told the author the
// step is fine.
func TestIncludedPluginStep_UncheckedIsReportedNotSilent(t *testing.T) {
	root := includeTree(t, goodParams)

	for _, tc := range []struct {
		mode string
		run  func() (int, string)
	}{
		{"validate-scenario", func() (int, string) { return runScenario(t, root, nil) }},
		{"validate-service-tree", func() (int, string) {
			code, out, errOut := runTree(t, TreeOptions{Root: root, ServiceName: "redis"})
			if errOut != "" {
				t.Fatalf("soul-lint failed to run: %s", errOut)
			}
			return code, out
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			code, out := tc.run()
			// A hint, so the run still passes: the author of a definition usually
			// cannot produce somebody else's manifest.
			if code != ExitOK {
				t.Fatalf("exit = %d, want %d — an unbound manifest is a hint, not a failure\n%s", code, ExitOK, out)
			}
			if !containsDiagFor(out, filepath.Join("scenario", "provision.yml"), "plugin_params_unchecked") {
				t.Fatalf("silence: nothing said about the included plugin step, at the body's own file\n%s", out)
			}
		})
	}
}

// And with the manifest bound the check is REAL — the same `unknown_param` an
// inline step gets, at the included file's own line.
func TestIncludedPluginStep_ParamsAreCheckedAgainstTheManifest(t *testing.T) {
	root := includeTree(t, bogusParams)
	modules := bindVM(t)

	for _, tc := range []struct {
		mode string
		run  func() (int, string)
	}{
		{"validate-scenario", func() (int, string) { return runScenario(t, root, modules) }},
		{"validate-service-tree", func() (int, string) {
			code, out, errOut := runTree(t, TreeOptions{Root: root, Modules: modules, ServiceName: "redis"})
			if errOut != "" {
				t.Fatalf("soul-lint failed to run: %s", errOut)
			}
			return code, out
		}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			code, out := tc.run()
			if code != ExitHasErrors {
				t.Fatalf("exit = %d, want %d — an undeclared param in an included body must fail the lint\n%s", code, ExitHasErrors, out)
			}
			if !containsDiagFor(out, filepath.Join("scenario", "provision.yml"), "unknown_param") {
				t.Errorf("the undeclared param is not reported at the body's own file\n%s", out)
			}
			if strings.Contains(out, "plugin_params_unchecked") {
				t.Errorf("reported the step as unchecked while checking it\n%s", out)
			}
		})
	}
}

// The required-param half of the same check, and the shape of the divergence
// NIM-763 hit: a param the module demands is simply absent from the included
// step.
func TestIncludedPluginStep_MissingRequiredParamIsReported(t *testing.T) {
	root := includeTree(t, "    name: redis\n")

	code, out := runScenario(t, root, bindVM(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("scenario", "provision.yml"), "missing_required_param") {
		t.Errorf("a required param missing from an included step was accepted\n%s", out)
	}
}

// A correct step against a bound manifest is quiet — neither the hint nor a
// finding. Without this the two above pass on an implementation that reports
// something unconditionally.
func TestIncludedPluginStep_CheckedAndCleanIsQuiet(t *testing.T) {
	root := includeTree(t, goodParams)

	code, out := runScenario(t, root, bindVM(t))

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}
	for _, code := range []string{"plugin_params_unchecked", "unknown_param", "missing_required_param"} {
		if strings.Contains(out, code) {
			t.Errorf("a step matching its manifest produced %s\n%s", code, out)
		}
	}
}

// The includER's own steps did not regress: a plugin step inline in main.yml is
// still checked by the per-document post-pass, and is not reported twice now that
// the expansion carries the body's diagnostics through as well.
func TestInlinePluginStep_StillCheckedExactlyOnce(t *testing.T) {
	root := includeTree(t, goodParams)
	main := filepath.Join(root, "scenario", "create", "main.yml")
	stageWrite(t, main, "name: create\ndescription: provisions inline and through an include\n\ntasks:\n"+
		"  - include: provision.yml\n"+
		"  - name: provision another batch\n    on: keeper\n    module: wb-cloud.vm.created\n    params:\n      count: 1\n      bogus_param: \"x\"\n")

	code, out := runScenario(t, root, bindVM(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	// The BRACKETED code, not the bare word: `deprecated_param`'s hint text ends
	// in "...is rejected as unknown_param", so a raw substring count would go to 2
	// the day a fixture param is marked deprecated, for no related reason.
	if n := strings.Count(out, "[unknown_param]"); n != 1 {
		t.Errorf("unknown_param reported %d times, want 1 — the inline step is checked by one pass, not two\n%s", n, out)
	}
	if !containsDiagFor(out, filepath.Join("scenario", "create", "main.yml"), "unknown_param") {
		t.Errorf("the inline step's finding lost its own file\n%s", out)
	}
}

// A body reached through a NESTED include is checked too. The expansion recurses,
// and a resolver that stopped at the first level would leave the deeper file in
// the silence this ticket closes — with nothing in the output to say so.
func TestIncludedPluginStep_NestedIncludeIsCheckedToo(t *testing.T) {
	root := includeTree(t, goodParams)
	stageWrite(t, filepath.Join(root, "scenario", "provision.yml"), "- include: cloud.yml\n")
	stageWrite(t, filepath.Join(root, "scenario", "cloud.yml"),
		"- name: provision the batch\n  on: keeper\n  module: wb-cloud.vm.created\n  params:\n"+bogusParams)

	code, out := runScenario(t, root, bindVM(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("scenario", "cloud.yml"), "unknown_param") {
		t.Errorf("the second-level body was not checked\n%s", out)
	}
}

// One body included TWICE — the dispatcher shape, two branches pulling one
// provision file — is read and validated twice, and must still be reported once.
//
// This is the load-bearing version of the count above: before the dedupe in
// [config.ExpandIncludes] this printed two byte-identical lines at the same
// coordinates, which is the habit the per-document hint dedupe already exists to
// avoid. Both severities, because they travel by different routes: the hint is
// new to the returned slice, the error is new to bodies entirely.
func TestIncludedPluginStep_BodyIncludedTwiceIsReportedOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  string
		modules func() []string
		want    string
		code    int
	}{
		{"unchecked hint", goodParams, func() []string { return nil }, "[plugin_params_unchecked]", ExitOK},
		{"real finding", bogusParams, func() []string { return bindVM(t) }, "[unknown_param]", ExitHasErrors},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := includeTree(t, tc.params)
			stageWrite(t, filepath.Join(root, "scenario", "create", "main.yml"),
				"name: create\ndescription: two branches, one body\n\ntasks:\n"+
					"  - include: provision.yml\n    when: input.mode == 'a'\n"+
					"  - include: provision.yml\n    when: input.mode == 'b'\n")

			code, out := runScenario(t, root, tc.modules())

			if code != tc.code {
				t.Fatalf("exit = %d, want %d\n%s", code, tc.code, out)
			}
			if n := strings.Count(out, tc.want); n != 1 {
				t.Errorf("%s reported %d times, want 1 — one body, one defect, one line\n%s", tc.want, n, out)
			}
		})
	}
}

// A plugin step nested in a `block:` inside an included body is checked too. The
// walk claims to be grammar-agnostic (shared/config/module_params_plugin.go), and
// the ticket named this as the way a step could still slip through; nothing pinned
// the claim across the include boundary.
func TestIncludedPluginStep_InsideABlockIsCheckedToo(t *testing.T) {
	root := includeTree(t, goodParams)
	stageWrite(t, filepath.Join(root, "scenario", "provision.yml"),
		"- name: the batch\n  block:\n"+
			"    - name: provision\n      module: wb-cloud.vm.created\n      params:\n        count: 1\n        bogus_param: \"x\"\n")

	code, out := runScenario(t, root, bindVM(t))

	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitHasErrors, out)
	}
	if !containsDiagFor(out, filepath.Join("scenario", "provision.yml"), "unknown_param") {
		t.Errorf("a step inside a block: inside an include was not checked\n%s", out)
	}
}
