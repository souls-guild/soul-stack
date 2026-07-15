package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// fakeCreateLoader is a mock [CreateScenarioLoader]: Load returns an artifact
// with LocalDir pointing at a test snapshot (where scenario/<name>/main.yml
// files live). No git stack — ResolveCreateScenarios scans LocalDir via
// artifact.ListScenarios.
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

// ReadFile satisfies [CreatePlanLoader] (ResolveCreatePlan via ValidateInput).
// Not called in bare/required/nil-loader cases; reads the scenario main file
// off the snapshot disk when execution reaches that point.
func (f *fakeCreateLoader) ReadFile(_ *artifact.ServiceArtifact, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(f.localDir, file))
}

// writeScenarioFile drops a scenario/<name>/main.yml into the snapshot directory.
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

// TestResolveCreateScenarios_OnlyFlagged proves the set is EXACTLY scenarios
// with `create: true` (Phase 2: the union with default `create` is removed).
// The name `create` without the flag is not eligible; with the flag it's
// eligible like any other.
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

// TestResolveCreateScenarios_CreateNotPrivileged proves a `create` scenario
// WITHOUT `create: true` doesn't land in the set (Phase 2: the name `create`
// is not privileged, the union is removed). A regression here means the
// back-compat union came back.
func TestResolveCreateScenarios_CreateNotPrivileged(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n") // no flag
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

// TestResolveCreateScenarios_EmptyWhenNoFlags proves a service with no
// `create: true` at all yields an EMPTY set (valid: caller treats it as a
// bare incarnation).
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

// TestResolveCreateScenarios_LoadError proves a snapshot load failure
// propagates (handler → 500/502) rather than the set being guessed.
func TestResolveCreateScenarios_LoadError(t *testing.T) {
	sentinel := errors.New("clone failed")
	_, err := ResolveCreateScenarios(context.Background(), &fakeCreateLoader{loadErr: sentinel}, artifact.ServiceRef{Name: "svc"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrap of sentinel", err)
	}
}

// TestValidateCreateScenarioChoice_EmptyHasScenarios_Required proves an empty
// choice against a NON-EMPTY set yields ErrCreateScenarioRequired (handler →
// 422): a choice is mandatory.
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

// TestValidateCreateScenarioChoice_EmptyNoScenarios_Bare proves an empty
// choice against an EMPTY set yields bare=true, name="" (caller creates a
// bare incarnation).
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

// TestValidateCreateScenarioChoice_FlaggedOK proves an explicit choice of a
// scenario with `create: true` passes and is returned as-is (bare=false).
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

// TestValidateCreateScenarioChoice_NotInSet proves choosing a scenario
// OUTSIDE the create set (operational add_user) yields
// ErrCreateScenarioNotEligible (handler → 422).
func TestValidateCreateScenarioChoice_NotInSet(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")
	writeScenarioFile(t, root, "add_user", "name: add_user\ncreate: false\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "add_user")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible", err)
	}
}

// TestValidateCreateScenarioChoice_CreateWithoutFlag_NotEligible proves an
// explicit choice of `create` that does NOT carry `create: true` yields
// ErrCreateScenarioNotEligible (Phase 2: the name `create` is no longer
// privileged). A regression here means the `chosen == CreateScenarioName →
// return` shortcut came back.
func TestValidateCreateScenarioChoice_CreateWithoutFlag_NotEligible(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ntasks: []\n") // no flag
	writeScenarioFile(t, root, "create_cluster", "name: create_cluster\ncreate: true\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "create")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible (create без флага не привилегирован)", err)
	}
}

// TestValidateCreateScenarioChoice_BadName proves a garbage/invalid scenario
// name yields ErrCreateScenarioNotEligible (never reach the resolve step with
// a traversal name).
func TestValidateCreateScenarioChoice_BadName(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")

	_, _, err := ValidateCreateScenarioChoice(context.Background(), &fakeCreateLoader{localDir: root}, artifact.ServiceRef{Name: "svc"}, "../etc")
	if !errors.Is(err, ErrCreateScenarioNotEligible) {
		t.Fatalf("err = %v, want ErrCreateScenarioNotEligible for bad name", err)
	}
}

// --- ResolveCreatePlan (R2: shared resolve for REST CreateTyped + MCP create) ---

// recordingPreflighter is an AssertPreflighter stub: records whether it was
// called (gotSpec) and optionally returns preErr. Used to check the
// PreflightAssert gate in ResolveCreatePlan.
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

// noPreflighter does NOT implement AssertPreflighter (no PreflightAssert
// method): mirrors a ScenarioStarter fake, where the gate becomes a no-op.
type noPreflighter struct{}

// TestResolveCreatePlan_NilLoader_StubPlan covers loader==nil (REST stub
// mode): plan = `create` / not bare / auto_create=true (legacy behavior, no
// set resolve), and PreflightAssert is STILL called (outside the loader
// branch, as in the original handlers) with the incarnation name and the
// default `create`.
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
	// PreflightAssert is called with the incarnation name and the stub `create` scenario.
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

// TestResolveCreatePlan_AssertFails_Propagated proves that when the
// preflighter returns ErrAssertFailed, ResolveCreatePlan propagates it
// (caller maps to 422) and returns no plan. nil-loader path
// (createScenario=`create`, autoCreate=true → gate active), no ValidateInput
// stack involved.
func TestResolveCreatePlan_AssertFails_Propagated(t *testing.T) {
	pf := &recordingPreflighter{preErr: ErrAssertFailed}
	_, err := ResolveCreatePlan(context.Background(), nil, pf, "redis-prod",
		artifact.ServiceRef{Name: "redis", Ref: "v1"}, "", nil, "archon-alice")
	if !errors.Is(err, ErrAssertFailed) {
		t.Fatalf("err = %v, want ErrAssertFailed (проброс из preflighter)", err)
	}
}

// TestResolveCreatePlan_NoPreflighter_NoOp proves that when the preflighter
// does NOT implement AssertPreflighter (ScenarioStarter fake), the gate is a
// no-op and the plan returns without error. A regression here would mean the
// type assertion failed / panicked.
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

// TestResolveCreatePlan_Bare_NoPreflight proves an empty set (no create:true)
// plus an empty choice yields a bare plan (BareNoScenario=true),
// PreflightAssert is NOT called (gate is !bare), and ValidateInput is also
// skipped (no ReadFile call — confirmed by fakeCreateLoader having no ReadFile).
func TestResolveCreatePlan_Bare_NoPreflight(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "restart", "name: restart\ntasks: []\n") // no create:true

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

// TestResolveCreatePlan_Required_Error proves a non-empty set (create:true)
// plus an empty choice yields ErrCreateScenarioRequired (parity with
// ValidateCreateScenarioChoice), and PreflightAssert is NOT called (the error
// happens before the gate).
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
