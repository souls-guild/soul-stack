package scenario

import "testing"

// TestValidScenarioName_Positive — canonical snake_case scenario name that
// resolves to path `scenario/<name>/main.yml`.
func TestValidScenarioName_Positive(t *testing.T) {
	for _, name := range []string{"create", "add_acl_user", "update_config"} {
		if !ValidScenarioName(name) {
			t.Errorf("ValidScenarioName(%q) = false, want true", name)
		}
	}
}

// TestValidScenarioName_Negative — path-traversal guard: the name is
// validated BEFORE path resolution, rejecting `/`, `..`, uppercase, dots,
// and the empty string.
func TestValidScenarioName_Negative(t *testing.T) {
	for _, name := range []string{
		"../etc",   // path-traversal
		"Create",   // uppercase
		"add.user", // dot
		"a/b",      // slash
		"",         // empty
	} {
		if ValidScenarioName(name) {
			t.Errorf("ValidScenarioName(%q) = true, want false", name)
		}
	}
}

// TestLifecycleScenarioNames — the canonical catalog constant holds exactly
// two lifecycle names (create / destroy) and stays consistent with the
// per-name constants. Guards against silent drift, since DTO kind tagging
// and run special-casing both depend on this set.
func TestLifecycleScenarioNames(t *testing.T) {
	want := map[string]bool{
		CreateScenarioName:  true,
		DestroyScenarioName: true,
	}
	if len(LifecycleScenarioNames) != len(want) {
		t.Fatalf("LifecycleScenarioNames size = %d, want %d", len(LifecycleScenarioNames), len(want))
	}
	for name := range want {
		if !IsLifecycleScenario(name) {
			t.Errorf("IsLifecycleScenario(%q) = false, want true", name)
		}
	}
	if IsLifecycleScenario("add_replicas") {
		t.Error("IsLifecycleScenario(add_replicas) = true, want false")
	}
}

// TestIsRunnableScenario — canon for the runnable flag (ADR-042 "dumb
// frontend"): create=true (bootstrap run), destroy=false (deletion is a
// special DELETE flow, not a run), converge/any operational=true. Guards
// against drift in the scenario catalog tagging the UI uses to filter the
// Run form.
func TestIsRunnableScenario(t *testing.T) {
	want := map[string]bool{
		CreateScenarioName:   true,
		DestroyScenarioName:  false,
		ConvergeScenarioName: true,
		"rotate_certs":       true,
		"add_replicas":       true,
	}
	for name, exp := range want {
		if got := IsRunnableScenario(name); got != exp {
			t.Errorf("IsRunnableScenario(%q) = %v, want %v", name, got, exp)
		}
	}
}

// TestConvergeIsOperational — guard (amend ADR-031, 2026-06-10): `converge`
// was removed from the lifecycle set and is treated as an operational
// scenario kind (apply-reconcile via a normal run + dry-run check-drift
// target). Regression guard against converge sneaking back into
// LifecycleScenarioNames.
func TestConvergeIsOperational(t *testing.T) {
	if IsLifecycleScenario(ConvergeScenarioName) {
		t.Errorf("IsLifecycleScenario(%q) = true, want false (converge — operational, amend ADR-031)", ConvergeScenarioName)
	}
	if _, ok := LifecycleScenarioNames[ConvergeScenarioName]; ok {
		t.Errorf("converge (%q) не должен входить в LifecycleScenarioNames", ConvergeScenarioName)
	}
}

// TestScenarioRelPath — selects the load channel for the scenario's main
// YAML (ADR-0068 §3): fromUpgrade=true → upgrade/<name>/main.yml (second
// channel), false → scenario/<name>/main.yml (default behavior). Guards the
// switch that parseScenarioFromArtifact passes to loader.ReadFile.
func TestScenarioRelPath(t *testing.T) {
	tests := []struct {
		name        string
		scenario    string
		fromUpgrade bool
		want        string
	}{
		{"scenario-default", "create", false, "scenario/create/main.yml"},
		{"upgrade-channel", "v2", true, "upgrade/v2/main.yml"},
		{"scenario-default-op", "add_user", false, "scenario/add_user/main.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scenarioRelPath(tt.scenario, tt.fromUpgrade); got != tt.want {
				t.Fatalf("scenarioRelPath(%q, %v) = %q, want %q", tt.scenario, tt.fromUpgrade, got, tt.want)
			}
		})
	}
}
