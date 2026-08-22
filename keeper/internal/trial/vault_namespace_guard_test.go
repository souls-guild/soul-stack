package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
// The path is assembled through `vars` rather than written into the call, so the
// load-time scan has nothing to compare and only the evaluated-path guard can
// catch it — the layer that is switched off with the service name.
func TestRunCase_OwnNamespaceVaultFencedAtL0(t *testing.T) {
	mainYML := `name: create
tasks:
  - name: read own namespace through a variable
    module: core.file.present
    vars:
      ref: "secret/fenced/prod/db#password"
    params:
      path: /tmp/x
      content: "${ vault(vars.ref) }"
`
	caseDir := writeScenarioTree(t, mainYML, `name: own-namespace vault is fenced
fixtures:
  vault:
    secret/fenced/prod/db:
      password: leaked
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	// service.yml is what supplies the identity; the fence compares against its
	// `name:`, not against the directory the service happens to sit in.
	svcRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(caseDir))))
	if err := os.WriteFile(filepath.Join(svcRoot, "service.yml"), []byte(`name: fenced
state_schema_version: 1
state_schema:
  type: object
`), 0o644); err != nil {
		t.Fatalf("write service.yml: %v", err)
	}

	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("expected the render to be refused: vault() reached secret/fenced/, the namespace the platform derives")
	}
	if !strings.Contains(err.Error(), "vault_path_in_own_namespace") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
