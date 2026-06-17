package scenario

import "testing"

// TestValidScenarioName_Positive — каноническая snake_case-форма имени
// scenario, резолвящегося в путь `scenario/<name>/main.yml`.
func TestValidScenarioName_Positive(t *testing.T) {
	for _, name := range []string{"create", "add_acl_user", "update_config"} {
		if !ValidScenarioName(name) {
			t.Errorf("ValidScenarioName(%q) = false, want true", name)
		}
	}
}

// TestValidScenarioName_Negative — path-traversal guard: имя валидируется
// ДО резолва пути, отсекая `/`, `..`, верхний регистр, точку и пустую строку.
func TestValidScenarioName_Negative(t *testing.T) {
	for _, name := range []string{
		"../etc",   // path-traversal
		"Create",   // верхний регистр
		"add.user", // точка
		"a/b",      // слэш
		"",         // пусто
	} {
		if ValidScenarioName(name) {
			t.Errorf("ValidScenarioName(%q) = true, want false", name)
		}
	}
}

// TestLifecycleScenarioNames — каноническая константа-каталог содержит ровно
// два lifecycle-имени (create / destroy) и согласована с per-name константами.
// Защита от тихого дрейфа набора (DTO-разметка kind и спец-обработка прогона
// зависят от него).
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

// TestIsRunnableScenario — канон признака runnable (ADR-042 «тупой фронт»):
// create=true (bootstrap-run), destroy=false (удаление — спец-флоу DELETE, не
// run), converge/любой operational=true. Защита от дрейфа разметки каталога
// scenario, по которой UI фильтрует Run-форму.
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
// выведен из lifecycle-набора и трактуется как operational scenario-kind
// (Apply-reconcile через обычный run + dry-run target check-drift). Регресс-
// защита от возврата converge в LifecycleScenarioNames.
func TestConvergeIsOperational(t *testing.T) {
	if IsLifecycleScenario(ConvergeScenarioName) {
		t.Errorf("IsLifecycleScenario(%q) = true, want false (converge — operational, amend ADR-031)", ConvergeScenarioName)
	}
	if _, ok := LifecycleScenarioNames[ConvergeScenarioName]; ok {
		t.Errorf("converge (%q) не должен входить в LifecycleScenarioNames", ConvergeScenarioName)
	}
}
