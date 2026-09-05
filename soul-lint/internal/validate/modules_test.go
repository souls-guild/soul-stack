package validate

// `soul-lint --modules <alias>=<path>` end to end, through the subcommand.
//
// The loader itself moved to `shared/definition` in NIM-790 and is tested there —
// `soul-trial` grew the same flag and two copies of a CLI contract is how the two
// tools start disagreeing about what a binding means. What is tested HERE is the
// half that is soul-lint's: which exit code each outcome produces, and that an
// unreadable binding never degrades into the `unchecked` hint plus an `OK:` line.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/definition/deftest"
)

// writeScenario lays out `<root>/svc/scenario/add-user/main.yml` with one task
// addressing the fixture module and the given params block.
func writeScenario(t *testing.T, root string, params deftest.Params) string {
	t.Helper()
	dir := filepath.Join(root, "svc", "scenario", "add-user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "name: add-user\ntasks:\n  - name: t\n    module: " + deftest.Address + "\n    params:\n"
	for _, kv := range params {
		body += "      " + kv + "\n"
	}
	p := filepath.Join(dir, "main.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return p
}

// End to end through the subcommand: the point of the flag is that an undeclared param
// on a plugin module fails HERE, with a line, instead of on the host as
// module.unknown_param (ADR-0076(t)).
func TestRun_BoundSchemaCatchesUndeclaredParam(t *testing.T) {
	dir := t.TempDir()
	doc := deftest.WriteSchemaFile(t, dir, "soul-mod-redis", deftest.SchemaJSON)
	scenario := writeScenario(t, dir, deftest.Undeclared)

	var out, errOut bytes.Buffer
	code := Run(Options{Path: scenario, Kind: KindScenario, Modules: []string{deftest.Alias + "=" + doc}}, &out, &errOut)
	if code != ExitHasErrors {
		t.Fatalf("exit %d, want %d; stdout:\n%s", code, ExitHasErrors, out.String())
	}
	if !strings.Contains(out.String(), deftest.UndeclaredParam) {
		t.Errorf("a param the schema does not declare was accepted:\n%s", out.String())
	}
}

// The same run without the binding must not print a clean bill of health.
func TestRun_WithoutBindingReportsUnchecked(t *testing.T) {
	dir := t.TempDir()
	scenario := writeScenario(t, dir, deftest.Declared)

	var out, errOut bytes.Buffer
	code := Run(Options{Path: scenario, Kind: KindScenario}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit %d, want %d — an unbound plugin is not the author's error; stdout:\n%s", code, ExitOK, out.String())
	}
	if !strings.Contains(out.String(), deftest.Unchecked) {
		t.Errorf("no notice that nothing was checked:\n%s", out.String())
	}
}

// A binding that cannot be read stops the run at exit 2. It must never degrade into the
// unchecked hint and an `OK:` line — that is "checked and clean" written over "could not
// look", the failure this whole flag exists to remove.
func TestRun_UnreadableBindingIsFatalNotUnchecked(t *testing.T) {
	dir := t.TempDir()
	scenario := writeScenario(t, dir, deftest.Declared)

	var out, errOut bytes.Buffer
	code := Run(Options{
		Path: scenario, Kind: KindScenario,
		Modules: []string{deftest.Alias + "=" + filepath.Join(dir, "gone", "schema.json")},
	}, &out, &errOut)
	if code != ExitIOFatal {
		t.Fatalf("exit %d, want %d (I/O fatal)", code, ExitIOFatal)
	}
	if strings.Contains(out.String(), "OK:") {
		t.Errorf("printed a clean result for a run whose schema never loaded:\n%s", out.String())
	}
}

// validate-manifest reads a schema document now — both carriers, told apart by content.
func TestRun_ValidateManifestReadsBothCarriers(t *testing.T) {
	dir := t.TempDir()
	file := deftest.WriteSchemaFile(t, dir, "published", deftest.SchemaJSON)
	art := deftest.WriteStampedArtifact(t, filepath.Join(dir, "soul-mod-redis"), deftest.SchemaJSON)

	for _, p := range []string{file, art} {
		var out, errOut bytes.Buffer
		if code := Run(Options{Path: p, Kind: KindManifest}, &out, &errOut); code != ExitOK {
			t.Errorf("validate-manifest %s: exit %d\nstdout:\n%s\nstderr:\n%s", p, code, out.String(), errOut.String())
		}
	}
}

// An artifact that was never stamped has no disclosure to approve, and the finding says
// so rather than "this file is not JSON".
func TestRun_ValidateManifestFailsClosedOnUnstampedArtifact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "soul-mod-redis")
	if err := os.WriteFile(p, []byte("\x7fELF never stamped"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := Run(Options{Path: p, Kind: KindManifest}, &out, &errOut); code != ExitHasErrors {
		t.Fatalf("exit %d, want %d\nstdout:\n%s", code, ExitHasErrors, out.String())
	}
	if !strings.Contains(out.String(), "schema_trailer_missing") {
		t.Errorf("an unstamped artifact was not reported as unstamped:\n%s", out.String())
	}
}
