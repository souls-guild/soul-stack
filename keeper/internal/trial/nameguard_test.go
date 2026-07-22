package trial

import (
	"context"
	"testing"
)

// nameGuardMain — synthetic scenario with guard-assert on incarnation name form
// (mirror of NIM-58 provision-guard). Checks that fixtures.incarnation_name flows
// into incarnation.name in L0-render.
const nameGuardMain = `name: create
tasks:
  - name: guard incarnation name
    assert:
      that:
        - "incarnation.name.matches('^[a-z][a-z0-9-]{0,48}[a-z0-9]$')"
      message: "incarnation name does not fit as base VM name"
  - name: write marker
    module: core.file.present
    params:
      path: /tmp/soul-stack-marker
      content: ok
`

// TestRunCase_IncarnationNameOverride_BadNameAborts — override incarnation_name to
// «bad» name (starts with digit) aborts render on guard-assert. Proves that the field
// really controls incarnation.name: scn.Name="create" is valid, without override
// render would pass.
func TestRunCase_IncarnationNameOverride_BadNameAborts(t *testing.T) {
	caseDir := writeScenarioTree(t, nameGuardMain, `name: bad incarnation name aborts render
fixtures:
  incarnation_name: 9redis
expect_render_error: "incarnation name does not fit as base VM name"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (render aborted on guard-assert): %v", results[0].Failures)
	}
}

// TestRunCase_IncarnationNameOverride_GoodNameRenders — override to valid name
// passes guard-assert, plan renders.
func TestRunCase_IncarnationNameOverride_GoodNameRenders(t *testing.T) {
	caseDir := writeScenarioTree(t, nameGuardMain, `name: valid incarnation name renders
fixtures:
  incarnation_name: redis-prod
assert:
  task_present:
    - module: core.file.present
      params_subset:
        path: /tmp/soul-stack-marker
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (valid name, plan renders): %v", results[0].Failures)
	}
}
