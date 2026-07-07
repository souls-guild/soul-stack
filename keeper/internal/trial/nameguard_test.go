package trial

import (
	"context"
	"testing"
)

// nameGuardMain — синтетический сценарий с guard-assert на форму имени инкарнации
// (зеркало NIM-58 provision-guard). Проверяет проброс fixtures.incarnation_name в
// incarnation.name L0-рендера.
const nameGuardMain = `name: create
tasks:
  - name: guard incarnation name
    assert:
      that:
        - "incarnation.name.matches('^[a-z][a-z0-9-]{0,48}[a-z0-9]$')"
      message: "имя инкарнации не подходит как базовое имя VM"
  - name: write marker
    module: core.file.present
    params:
      path: /tmp/soul-stack-marker
      content: ok
`

// TestRunCase_IncarnationNameOverride_BadNameAborts — override incarnation_name на
// «плохое» имя (старт-цифра) обрывает render на guard-assert. Доказывает, что поле
// реально управляет incarnation.name: scn.Name="create" валиден, без override
// render прошёл бы.
func TestRunCase_IncarnationNameOverride_BadNameAborts(t *testing.T) {
	caseDir := writeScenarioTree(t, nameGuardMain, `name: bad incarnation name aborts render
fixtures:
  incarnation_name: 9redis
expect_render_error: "имя инкарнации не подходит как базовое имя VM"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (render оборвался на guard-assert): %v", results[0].Failures)
	}
}

// TestRunCase_IncarnationNameOverride_GoodNameRenders — override на валидное имя
// проходит guard-assert, план рендерится.
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
		t.Fatalf("ожидали PASS (валидное имя, план рендерится): %v", results[0].Failures)
	}
}
