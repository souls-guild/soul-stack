package validate

// NIM-790: `validate-scenario` and `validate-destiny` reach the SAME verdict on
// the same definition, because they now reach it through the same code.
//
// They did not. The scenario command grew the wiring in NIM-779 and the destiny
// command in NIM-783, as two implementations of one sequence — parse with the
// manifests bound, expand with the same resolver, carry every diagnostic out — and
// a third tool (`soul-trial`) had none of it and printed PASS over unchecked
// params. Counting findings per command would not have caught that: each command
// was self-consistently reporting its own subset. What catches it is the
// COMPARISON, over one input.
//
// The input is [deftest], shared with `shared/definition` and `keeper/internal/
// trial`. The three tools are three Go modules and cannot meet in one test
// process; the fixture is what they meet in.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/definition/deftest"
)

// writeAgreementScenario lays out a service tree whose scenario holds the
// canonical plugin step inline and behind an `include:`.
func writeAgreementScenario(t *testing.T, root string, p deftest.Params) string {
	t.Helper()
	svc := filepath.Join(root, "svc")
	dir := filepath.Join(svc, "scenario", "create")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(svc, "service.yml"), "description: agreement fixture\n\nstate_schema: {}\n")
	write(filepath.Join(dir, "main.yml"), deftest.ScenarioMain("create", "body.yml", p))
	write(filepath.Join(dir, "body.yml"), deftest.TaskList("included step", p))
	return filepath.Join(dir, "main.yml")
}

// writeAgreementDestiny lays out a destiny artifact holding the same two steps.
func writeAgreementDestiny(t *testing.T, root string, p deftest.Params) string {
	t.Helper()
	dst := filepath.Join(root, "dst")
	tasks := filepath.Join(dst, "tasks")
	if err := os.MkdirAll(tasks, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(dst, "destiny.yml"), "name: acl\ndescription: agreement fixture\n")
	write(filepath.Join(tasks, "main.yml"), deftest.DestinyTasksMain("body.yml", p))
	write(filepath.Join(tasks, "body.yml"), deftest.TaskList("included step", p))
	return filepath.Join(dst, "destiny.yml")
}

// pluginFindings counts the bracketed plugin-params codes in a report.
//
// The BRACKETED code, not the bare word: `deprecated_param`'s hint text ends in
// "...is rejected as unknown_param", so a raw substring count would go up the day
// a fixture param is marked deprecated, for no related reason.
func pluginFindings(out string) map[string]int {
	got := map[string]int{}
	for _, code := range []string{deftest.Unchecked, deftest.UndeclaredParam, deftest.MissingParam} {
		if n := strings.Count(out, "["+code+"]"); n > 0 {
			got[code] = n
		}
	}
	return got
}

func runValidate(t *testing.T, path string, kind Kind, modules []string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: kind, Modules: modules, ServiceName: "svc"}, &out, &errOut)
	if errOut.Len() > 0 {
		t.Fatalf("soul-lint failed to run: %s", errOut.String())
	}
	return code, out.String()
}

// ★ One input, two commands, one verdict.
func TestScenarioAndDestinyAgreeOnPluginParams(t *testing.T) {
	for _, tc := range []struct {
		name     string
		params   deftest.Params
		bind     bool
		wantCode int
		want     map[string]int
	}{
		{
			// Nobody bound a manifest: both commands say so, twice each — once for
			// the step in the entry file, once for the step behind the include —
			// and neither fails, because the author of a definition usually cannot
			// produce somebody else's plugin's manifest.
			name: "unbound", params: deftest.Declared, bind: false,
			wantCode: ExitOK,
			want:     map[string]int{deftest.Unchecked: 2},
		},
		{
			name: "undeclared param", params: deftest.Undeclared, bind: true,
			wantCode: ExitHasErrors,
			want:     map[string]int{deftest.UndeclaredParam: 2},
		},
		{
			name: "missing required param", params: deftest.MissingRequired, bind: true,
			wantCode: ExitHasErrors,
			want:     map[string]int{deftest.MissingParam: 2},
		},
		{
			name: "checked and clean", params: deftest.Declared, bind: true,
			wantCode: ExitOK,
			want:     map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var modules []string
			if tc.bind {
				modules = []string{deftest.Binding(t)}
			}

			scnCode, scnOut := runValidate(t, writeAgreementScenario(t, root, tc.params), KindScenario, modules)
			dstCode, dstOut := runValidate(t, writeAgreementDestiny(t, root, tc.params), KindDestiny, modules)

			scn, dst := pluginFindings(scnOut), pluginFindings(dstOut)
			if !sameCounts(scn, dst) {
				t.Errorf("the two commands disagree on one input:\n  validate-scenario: %v\n  validate-destiny:  %v\n\n%s\n%s",
					scn, dst, scnOut, dstOut)
			}
			if !sameCounts(scn, tc.want) {
				t.Errorf("validate-scenario reported %v, want %v\n%s", scn, tc.want, scnOut)
			}
			if scnCode != tc.wantCode {
				t.Errorf("validate-scenario exit %d, want %d\n%s", scnCode, tc.wantCode, scnOut)
			}
			if dstCode != tc.wantCode {
				t.Errorf("validate-destiny exit %d, want %d\n%s", dstCode, tc.wantCode, dstOut)
			}
		})
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
