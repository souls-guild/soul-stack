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

// TestResolveCreateScenarios_FlaggedPlusDefault — набор = сценарии с `create: true`
// ∪ {default `create`} (back-compat: имя `create` всегда годно как стартовое, даже
// без явного флага).
func TestResolveCreateScenarios_FlaggedPlusDefault(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n") // без флага — но default-конвенция
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

// TestResolveCreateScenarios_DefaultAlwaysPresent — даже если сценария `create` нет
// в снапшоте, default-имя включается в набор (back-compat: handler не должен 422-ить
// дефолтный create-запрос на старом сервисе без флагов; реальная проверка наличия
// файла остаётся за прогоном).
func TestResolveCreateScenarios_DefaultAlwaysPresent(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n")

	set, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"})
	if err != nil {
		t.Fatalf("ResolveCreateScenarios: %v", err)
	}
	if _, ok := set[CreateScenarioName]; !ok {
		t.Errorf("default %q должен быть в наборе всегда; set=%v", CreateScenarioName, set)
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

// TestValidateCreateScenarioChoice_DefaultEmpty — пустой выбор резолвится в default
// `create` (back-compat).
func TestValidateCreateScenarioChoice_DefaultEmpty(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n")
	name, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "")
	if err != nil {
		t.Fatalf("ValidateCreateScenarioChoice(\"\"): %v", err)
	}
	if name != CreateScenarioName {
		t.Fatalf("resolved name = %q, want %q", name, CreateScenarioName)
	}
}

// TestValidateCreateScenarioChoice_FlaggedOK — явный выбор сценария с `create: true`
// проходит и возвращается как есть.
func TestValidateCreateScenarioChoice_FlaggedOK(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n")
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")
	name, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "create_cluster")
	if err != nil {
		t.Fatalf("ValidateCreateScenarioChoice(create_cluster): %v", err)
	}
	if name != "create_cluster" {
		t.Fatalf("resolved name = %q, want create_cluster", name)
	}
}

// TestValidateCreateScenarioChoice_NotInSet — выбор сценария ВНЕ create-набора
// (operational add_user) → ErrCreateScenarioNotEligible (handler → 422).
func TestValidateCreateScenarioChoice_NotInSet(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n")
	writeScenarioFile(t, root, "add_user", "name: add_user\ncreate: false\ntasks: []\n")
	_, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "add_user")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible", err)
	}
}

// TestValidateCreateScenarioChoice_BadName — мусорное/невалидное имя сценария →
// ErrCreateScenarioNotEligible (не лезем в резолв с traversal-именем).
func TestValidateCreateScenarioChoice_BadName(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n")
	_, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "../etc")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible for bad name", err)
	}
}
