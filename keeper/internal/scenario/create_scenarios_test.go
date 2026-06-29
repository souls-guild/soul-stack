package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeCreateLoader — мок [CreateScenarioLoader]: Load отдаёт артефакт с LocalDir,
// указывающим на тестовый снапшот (где разложены scenario/<name>/main.yml). Без
// git-стека — ResolveCreateScenarios сканирует LocalDir через artifact.ListScenarios.
type fakeCreateLoader struct {
	localDir string
	loadErr  error
}

func (f *fakeCreateLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return &artifact.ServiceArtifact{Ref: ref, LocalDir: f.localDir}, nil
}

// writeScenarioFile подкладывает scenario/<name>/main.yml в снапшот-каталог.
func writeScenarioFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "scenario", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.yml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestResolveCreateScenarios_OnlyFlagged — набор = РОВНО сценарии с `create: true`
// (Фаза 2: union с дефолтным `create` убран). Имя `create` без флага НЕ годно;
// `create` с флагом — годно как любой другой.
func TestResolveCreateScenarios_OnlyFlagged(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "add_user", "name: add_user\ncreate: false\ntasks: []\n")
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n")

	set, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"})
	if err != nil {
		t.Fatalf("ResolveCreateScenarios: %v", err)
	}
	for _, want := range []string{"create", "create_cluster"} {
		if _, ok := set[want]; !ok {
			t.Errorf("create-набор должен содержать %q; set=%v", want, set)
		}
	}
	for _, notWant := range []string{"add_user", "restart"} {
		if _, ok := set[notWant]; ok {
			t.Errorf("create-набор НЕ должен содержать %q; set=%v", notWant, set)
		}
	}
}

// TestResolveCreateScenarios_CreateNotPrivileged — сценарий `create` БЕЗ
// `create: true` НЕ попадает в набор (Фаза 2: имя `create` не привилегировано,
// union убран). Регресс = back-compat-union вернулся.
func TestResolveCreateScenarios_CreateNotPrivileged(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n") // без флага
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n")

	set, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"})
	if err != nil {
		t.Fatalf("ResolveCreateScenarios: %v", err)
	}
	if _, ok := set[CreateScenarioName]; ok {
		t.Errorf("`create` без флага create:true НЕ должен быть в наборе; set=%v", set)
	}
	if len(set) != 0 {
		t.Errorf("набор должен быть пуст (нет create:true); set=%v", set)
	}
}

// TestResolveCreateScenarios_EmptyWhenNoFlags — сервис без единого `create: true`
// → ПУСТОЙ набор (валидно: caller трактует как bare-инкарнацию).
func TestResolveCreateScenarios_EmptyWhenNoFlags(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n")
	writeScenarioFile(t, root, "add_user", "name: add_user\ntasks: []\n")

	set, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"})
	if err != nil {
		t.Fatalf("ResolveCreateScenarios: %v", err)
	}
	if len(set) != 0 {
		t.Errorf("набор должен быть пуст (нет create-сценариев); set=%v", set)
	}
}

// TestResolveCreateScenarios_LoadError — сбой загрузки снапшота пробрасывается
// (handler → 500/502), набор не угадывается.
func TestResolveCreateScenarios_LoadError(t *testing.T) {
	sentinel := errors.New("clone failed")
	_, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{loadErr: sentinel}, artifact.ServiceRef{Name: "svc"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrap of sentinel", err)
	}
}

// TestValidateCreateScenarioChoice_EmptyHasScenarios_Required — пустой выбор при
// НЕПУСТОМ наборе → ErrCreateScenarioRequired (handler → 422): выбор обязателен.
func TestValidateCreateScenarioChoice_EmptyHasScenarios_Required(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")

	name, bare, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "")
	if !errors.Is(err, ErrCreateScenarioRequired) {
		t.Fatalf("err = %v, want ErrCreateScenarioRequired", err)
	}
	if bare {
		t.Errorf("bare = true, want false (набор непуст)")
	}
	if name != "" {
		t.Errorf("name = %q, want \"\"", name)
	}
}

// TestValidateCreateScenarioChoice_EmptyNoScenarios_Bare — пустой выбор при
// ПУСТОМ наборе → bare=true, name="" (caller создаёт bare-инкарнацию).
func TestValidateCreateScenarioChoice_EmptyNoScenarios_Bare(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n")

	name, bare, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "")
	if err != nil {
		t.Fatalf("ValidateCreateScenarioChoice(\"\", no-scenarios): %v", err)
	}
	if !bare {
		t.Errorf("bare = false, want true (нет create-сценариев)")
	}
	if name != "" {
		t.Errorf("name = %q, want \"\" (bare)", name)
	}
}

// TestValidateCreateScenarioChoice_FlaggedOK — явный выбор сценария с `create: true`
// проходит и возвращается как есть (bare=false).
func TestValidateCreateScenarioChoice_FlaggedOK(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")

	name, bare, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "create_cluster")
	if err != nil {
		t.Fatalf("ValidateCreateScenarioChoice(create_cluster): %v", err)
	}
	if bare {
		t.Errorf("bare = true, want false (явный выбор)")
	}
	if name != "create_cluster" {
		t.Fatalf("resolved name = %q, want create_cluster", name)
	}
}

// TestValidateCreateScenarioChoice_NotInSet — выбор сценария ВНЕ create-набора
// (operational add_user) → ErrCreateScenarioNotEligible (handler → 422).
func TestValidateCreateScenarioChoice_NotInSet(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "add_user", "name: add_user\ncreate: false\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "add_user")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible", err)
	}
}

// TestValidateCreateScenarioChoice_CreateWithoutFlag_NotEligible — явный выбор
// `create`, который НЕ несёт `create: true` → ErrCreateScenarioNotEligible
// (Фаза 2: имя `create` больше не привилегировано). Регресс = шорткат `chosen ==
// CreateScenarioName → return` вернулся.
func TestValidateCreateScenarioChoice_CreateWithoutFlag_NotEligible(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n") // без флага
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "create")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible (create без флага не привилегирован)", err)
	}
}

// TestValidateCreateScenarioChoice_BadName — мусорное/невалидное имя сценария →
// ErrCreateScenarioNotEligible (не лезем в резолв с traversal-именем).
func TestValidateCreateScenarioChoice_BadName(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "../etc")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible for bad name", err)
	}
}
