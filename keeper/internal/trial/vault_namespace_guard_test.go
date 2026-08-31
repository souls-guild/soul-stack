package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scenario body both halves of this guard run. The path is assembled through
// `vars` rather than written into the call, so the load-time scan has nothing to
// compare and only the evaluated-path guard can catch it — the layer that is
// switched off with the service name.
const fencedMainYML = `name: create
tasks:
  - name: read own namespace through a variable
    module: core.file.present
    vars:
      ref: "secret/fenced/prod/db#password"
    params:
      path: /tmp/x
      content: "${ vault(vars.ref) }"
`

const fencedAssert = `assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`

// TestRunCase_OwnNamespaceVaultFencedAtL0 is the guard on the trial harness
// carrying the service identity into the render pipeline ([ADR-0083] §7).
//
// Both halves of the fence are conditional on a non-empty service: an empty one
// disables the evaluated-path guard and the runtime scan alike. The harness used
// to leave RenderInput.Incarnation.Service unset, so every L0 case ran with the
// fence inert — a scenario reaching into its own derived namespace passed L0 and
// failed only on a live run. This case would go green again the moment that
// wiring is dropped.
//
// NIM-726 moved where the name comes from — the manifest no longer states one —
// so the guard now covers BOTH sources, because the point was never the manifest:
// it is that the name cannot be empty. The default source is the service
// directory, and `fixtures.service` overrides it when the directory is not the
// registered name.
func TestRunCase_OwnNamespaceVaultFencedAtL0(t *testing.T) {
	// The name is stated by the case. This is the path a service whose directory
	// is not its registered name takes.
	t.Run("from fixtures.service", func(t *testing.T) {
		caseDir := writeScenarioTree(t, fencedMainYML, `name: own-namespace vault is fenced
fixtures:
  service: fenced
  vault:
    secret/fenced/prod/db:
      password: leaked
`+fencedAssert)
		assertFencedAtL0(t, caseDir)
	})

	// No `fixtures.service`, and the service tree carries no name of its own: the
	// directory IS the identity. A service.yml is written to prove the manifest is
	// not consulted for this — it never mentions `fenced`.
	t.Run("from the service directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "fenced")
		caseDir := filepath.Join(root, "scenario", "create", "tests", "c1")
		if err := os.MkdirAll(caseDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFile(t, filepath.Join(root, "scenario", "create", "main.yml"), fencedMainYML)
		writeFile(t, filepath.Join(root, "service.yml"), "state_schema_version: 1\nstate_schema:\n  type: object\n")
		writeFile(t, filepath.Join(caseDir, caseFileName), `name: own-namespace vault is fenced
fixtures:
  vault:
    secret/fenced/prod/db:
      password: leaked
`+fencedAssert)
		assertFencedAtL0(t, caseDir)
	})
}

func assertFencedAtL0(t *testing.T, caseDir string) {
	t.Helper()
	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("expected the render to be refused: vault() reached secret/fenced/, the namespace the platform derives")
	}
	if !strings.Contains(err.Error(), "vault_path_in_own_namespace") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ★ The derivation must be a function of the file on disk, never of how the path
// was typed. serviceRootFor is four lexical Dir calls, so the shape soul-trial is
// handed when it runs from INSIDE a service repo — `scenario/<name>/tests/<case>/case.yml`
// — decomposes to "." and Base(".") is ".". That name is not empty, so it passes
// every `service == ""` guard, and PathAddressesOwnNamespace drops "." segments, so
// the fence it installs can never match: green offline, refused at render. Exactly
// the fail-open NIM-726 removed, re-entering through the path handling.
func TestTrialServiceName_RelativeCasePathStillNamesTheService(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	root := filepath.Join(t.TempDir(), "redis")
	if err := os.MkdirAll(filepath.Join(root, "scenario", "create", "tests", "c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	// The relative form, as typed from the service repo root.
	if got := trialServiceName(filepath.FromSlash("scenario/create/tests/c1/case.yml"), Fixtures{}); got != "redis" {
		t.Fatalf("relative case path gave service %q, want redis", got)
	}
	// And the absolute form agrees — the two spellings of one file must not differ.
	abs := filepath.Join(root, "scenario", "create", "tests", "c1", "case.yml")
	if got := trialServiceName(abs, Fixtures{}); got != "redis" {
		t.Fatalf("absolute case path gave service %q, want redis", got)
	}
}

// A standalone-destiny wrapper (`<destiny>/_trial/`) is not itself an identity: all
// four in-tree wrappers would otherwise be called `_trial`, one word shared by all of
// them and rejected by the registry's own name grammar. The destiny it wraps is the
// name, which is what the retired `name: trial-<destiny>` encoded.
func TestTrialServiceName_DestinyWrapperNamesTheDestiny(t *testing.T) {
	root := t.TempDir()
	caseFile := filepath.Join(root, "vector", "_trial", "scenario", "apply", "tests", "c1", "case.yml")
	if got := trialServiceName(caseFile, Fixtures{}); got != "vector" {
		t.Fatalf("wrapper case gave service %q, want vector", got)
	}
	// fixtures.service still wins over both rules.
	if got := trialServiceName(caseFile, Fixtures{Service: "stated"}); got != "stated" {
		t.Fatalf("fixtures.service = %q, want stated", got)
	}
}

// ★ A case that ABORTS still fenced on a name, and the report must say which. The
// identity is resolved at the top of renderCase for exactly this reason: every early
// `return rc, err` hands the caller the same struct, so resolving it late made the
// eight `expect_render_error` cases in the corpus report `fenced as service ""` — the
// report claiming the fence had no identity when it had one. An empty name is also
// precisely the string that switches the fence off, so a reader had no way to tell a
// reporting artefact from the real fail-open this ticket removed.
func TestRunCase_AbortedCaseStillReportsTheFencedService(t *testing.T) {
	root := filepath.Join(t.TempDir(), "redis")
	caseDir := filepath.Join(root, "scenario", "create", "tests", "c1")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// `required: true` with no value supplied — ResolveInputContract aborts the render,
	// well before the point the identity used to be resolved at.
	writeFile(t, filepath.Join(root, "scenario", "create", "main.yml"), `name: create
input:
  who: {type: string, required: true}
tasks:
  - name: noop
    module: core.exec.run
    params:
      cmd: "true"
`)
	writeFile(t, filepath.Join(caseDir, caseFileName), `name: aborts before render
expect_render_error: "who"
`)

	c, caseFile, err := LoadCase(filepath.Join(caseDir, caseFileName))
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	res, err := RunCase(context.Background(), c, caseFile)
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if res.Service != "redis" {
		t.Fatalf("aborted case reported service %q, want redis", res.Service)
	}
	if res.ServiceStated {
		t.Fatal("the name came from the directory; it must not be reported as stated")
	}
}
