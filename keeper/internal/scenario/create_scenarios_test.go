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

// ReadFile — для удовлетворения [CreatePlanLoader] (ResolveCreatePlan через
// ValidateInput). В bare/required/nil-loader-кейсах НЕ вызывается; читает
// scenario-main с диска снапшота, если до него дошло.
func (f *fakeCreateLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.localDir, file))
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

// --- ResolveCreatePlan (R2: общий резолв REST CreateTyped + MCP create) -----

// recordingPreflighter — стаб AssertPreflighter: учитывает факт вызова (gotSpec) и
// опц. возвращает preErr. Для проверки гейта PreflightAssert в ResolveCreatePlan.
type recordingPreflighter struct {
	called  bool
	gotSpec RunSpec
	preErr  error
}

func (p *recordingPreflighter) PreflightAssert(_ context.Context, spec RunSpec) error {
	p.called = true
	p.gotSpec = spec
	return p.preErr
}

// noPreflighter — НЕ реализует AssertPreflighter (нет метода PreflightAssert):
// зеркало ScenarioStarter-фейка, при котором гейт no-op.
type noPreflighter struct{}

// TestResolveCreatePlan_NilLoader_StubPlan — loader==nil (stub-режим REST): план =
// `create` / не bare / auto_create=true (legacy-поведение без резолва набора),
// PreflightAssert ВСЁ РАВНО зовётся (вне loader-ветки, как в исходных handler-ах) с
// именем инкарнации и дефолтным `create`.
func TestResolveCreatePlan_NilLoader_StubPlan(t *testing.T) {
	pf := &recordingPreflighter{}
	plan, err := ResolveCreatePlan(context.Background(), nil, pf, "redis-prod",
		artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", map[string]any{"x": 1}, "archon-alice")
	if err != nil {
		t.Fatalf("ResolveCreatePlan(nil loader): %v", err)
	}
	if plan.CreateScenario != CreateScenarioName {
		t.Errorf("CreateScenario = %q, want %q (stub-дефолт)", plan.CreateScenario, CreateScenarioName)
	}
	if plan.BareNoScenario {
		t.Error("BareNoScenario = true, want false (stub-план не bare)")
	}
	if !plan.AutoCreate {
		t.Error("AutoCreate = false, want true (stub-дефолт)")
	}
	// PreflightAssert вызван с именем инкарнации и stub-сценарием `create`.
	if !pf.called {
		t.Fatal("PreflightAssert НЕ вызван при nil-loader (регресс: исходный handler звал его вне loader-ветки)")
	}
	if pf.gotSpec.IncarnationName != "redis-prod" {
		t.Errorf("preflight IncarnationName = %q, want redis-prod", pf.gotSpec.IncarnationName)
	}
	if pf.gotSpec.ScenarioName != CreateScenarioName {
		t.Errorf("preflight ScenarioName = %q, want %q", pf.gotSpec.ScenarioName, CreateScenarioName)
	}
}

// TestResolveCreatePlan_AssertFails_Propagated — preflighter возвращает
// ErrAssertFailed → ResolveCreatePlan пробрасывает её (caller маппит в 422), план
// НЕ возвращается. nil-loader-путь (createScenario=`create`, autoCreate=true → гейт
// активен), без ValidateInput-стека.
func TestResolveCreatePlan_AssertFails_Propagated(t *testing.T) {
	pf := &recordingPreflighter{preErr: ErrAssertFailed}
	_, err := ResolveCreatePlan(context.Background(), nil, pf, "redis-prod",
		artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", nil, "archon-alice")
	if !errors.Is(err, ErrAssertFailed) {
		t.Fatalf("err = %v, want ErrAssertFailed (проброс из preflighter)", err)
	}
}

// TestResolveCreatePlan_NoPreflighter_NoOp — preflighter НЕ реализует
// AssertPreflighter (ScenarioStarter-фейк) → гейт no-op, план возвращается без
// ошибки. Регресс = type-assertion упал бы / panic.
func TestResolveCreatePlan_NoPreflighter_NoOp(t *testing.T) {
	plan, err := ResolveCreatePlan(context.Background(), nil, noPreflighter{}, "redis-prod",
		artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", nil, "archon-alice")
	if err != nil {
		t.Fatalf("ResolveCreatePlan(no preflighter): %v", err)
	}
	if plan.CreateScenario != CreateScenarioName || plan.BareNoScenario || !plan.AutoCreate {
		t.Errorf("plan = %+v, want {create, false, true}", plan)
	}
}

// TestResolveCreatePlan_Bare_NoPreflight — пустой набор (нет create:true) + пустой
// выбор → bare-план (BareNoScenario=true), PreflightAssert НЕ зовётся (гейт
// !bare). ValidateInput тоже пропущен (нет ReadFile-вызова — fakeCreateLoader без
// ReadFile это подтверждает).
func TestResolveCreatePlan_Bare_NoPreflight(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n") // нет create:true

	pf := &recordingPreflighter{}
	plan, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, pf,
		"redis-bare", artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", nil, "archon-alice")
	if err != nil {
		t.Fatalf("ResolveCreatePlan(bare): %v", err)
	}
	if !plan.BareNoScenario {
		t.Error("BareNoScenario = false, want true (пустой набор)")
	}
	if plan.CreateScenario != "" {
		t.Errorf("CreateScenario = %q, want \"\" (bare)", plan.CreateScenario)
	}
	if pf.called {
		t.Error("PreflightAssert вызван для bare-плана (гейт !bare нарушен)")
	}
}

// TestResolveCreatePlan_Required_Error — непустой набор (create:true) + пустой
// выбор → ErrCreateScenarioRequired (паритет ValidateCreateScenarioChoice),
// PreflightAssert НЕ зовётся (ошибка ДО гейта).
func TestResolveCreatePlan_Required_Error(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")

	pf := &recordingPreflighter{}
	_, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, pf,
		"redis-prod", artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", nil, "archon-alice")
	if !errors.Is(err, ErrCreateScenarioRequired) {
		t.Fatalf("err = %v, want ErrCreateScenarioRequired", err)
	}
	if pf.called {
		t.Error("PreflightAssert вызван при required-ошибке (должен быть отказ ДО гейта)")
	}
}
