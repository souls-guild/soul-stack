package trial

import (
	"context"
	"strings"
	"testing"
)

// A disruptive step gated on the window, and a note that reports the window.
const windowMain = `name: create
tasks:
  - name: restart
    module: core.service.restarted
    when: in_maintenance_window()
    params:
      name: redis-server
  - name: note
    module: core.file.present
    params:
      path: /tmp/window
      content: "${ incarnation.maintenance_window.text }|${ incarnation.maintenance_window.next_start }|${ in_maintenance_window() }"
`

func runWindowCase(t *testing.T, mainYML, caseYML string) Result {
	t.Helper()
	results, err := Run(context.Background(), writeScenarioTree(t, mainYML, caseYML), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	return results[0]
}

func TestMaintenanceWindow_L0Open(t *testing.T) {
	res := runWindowCase(t, windowMain, `name: open
fixtures:
  maintenance_window:
    open: true
    text: "mon 02:00 for 3h (Europe/Moscow)"
assert:
  task_present:
    - module: core.service.restarted
    - module: core.file.present
      params_subset:
        content: "mon 02:00 for 3h (Europe/Moscow)|2000-01-01T00:00:00Z|true"
`)
	if !res.Pass {
		t.Fatalf("expected PASS, got %v", res.Failures)
	}
}

func TestMaintenanceWindow_L0Closed(t *testing.T) {
	res := runWindowCase(t, windowMain, `name: closed
fixtures:
  maintenance_window:
    open: false
    text: "mon 02:00 for 3h (Europe/Moscow)"
    next_start: "2000-01-02T23:00:00Z"
assert:
  task_absent:
    - module: core.service.restarted
  task_present:
    - module: core.file.present
      params_subset:
        content: "mon 02:00 for 3h (Europe/Moscow)|2000-01-02T23:00:00Z|false"
`)
	if !res.Pass {
		t.Fatalf("expected PASS, got %v", res.Failures)
	}
}

// No windows: the step runs, the text is empty and the next start is now.
func TestMaintenanceWindow_L0None(t *testing.T) {
	res := runWindowCase(t, windowMain, `name: none
assert:
  task_present:
    - module: core.service.restarted
    - module: core.file.present
      params_subset:
        content: "|2000-01-01T00:00:00Z|true"
`)
	if !res.Pass {
		t.Fatalf("expected PASS, got %v", res.Failures)
	}
}

// changed_when runs after the module, later than the moment the window was read.
func TestMaintenanceWindow_L0RefusedInPostModulePredicate(t *testing.T) {
	res := runWindowCase(t, `name: create
tasks:
  - name: restart
    module: core.service.restarted
    changed_when: in_maintenance_window()
    params:
      name: redis-server
`, `name: refused
expect_render_error: "changed_when: CEL unsupported"
`)
	if !res.Pass {
		t.Fatalf("expected PASS (render refused), got %v", res.Failures)
	}
}

func TestMaintenanceWindow_FixtureValidation(t *testing.T) {
	for name, tc := range map[string]struct{ fixture, want string }{
		"open missing":       {"{text: x}", "open: required"},
		"bad next_start":     {"{open: false, next_start: tomorrow}", "next_start"},
		"open starts later":  {"{open: true, next_start: \"2000-01-02T00:00:00Z\"}", "an open window starts now"},
		"closed starts now":  {"{open: false}", "a closed window starts later"},
		"closed in the past": {"{open: false, next_start: \"1999-12-31T00:00:00Z\"}", "a closed window starts later"},
	} {
		t.Run(name, func(t *testing.T) {
			caseDir := writeScenarioTree(t, windowMain, "name: x\nfixtures:\n  maintenance_window: "+tc.fixture+"\nassert:\n  task_present:\n    - module: core.file.present\n")
			_, err := Run(context.Background(), caseDir, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}
