package scenario

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// traitsServiceRepo is a service whose vars read `incarnation.traits`, with one
// scenario per assert over them.
func traitsServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	files := map[string]string{
		"service.yml": "description: traits pre-flight guard\nstate_schema: {}\n",
		"vars/00-base.yaml": "team: \"${ default(incarnation.traits.team, 'none') }\"\n" +
			"has_traits: \"${ has(incarnation.traits) }\"\n",
	}
	for name, that := range map[string]string{
		"team_defaults": "vars.team == 'none'",
		"has_traits":    "vars.has_traits",
		"team_is_ops":   "vars.team == 'ops'",
	} {
		files["scenario/"+name+"/main.yml"] = "name: " + name + "\ndescription: asserts over traits\ntasks:\n" +
			"  - name: the assert\n    assert:\n      that:\n        - \"" + that + "\"\n" +
			"  - name: echo\n    module: core.exec.run\n    params:\n      cmd: echo\n    changed_when: \"false\"\n"
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestPreflightAssert_CreateWithoutTraitsSeesAnEmptyTraitsMap — a create request
// without traits has no row to read, so pre-flight builds the incarnation from the
// request and its traits are nil. The run reads the NOT NULL DEFAULT '{}' column, so a
// service var over `incarnation.traits` must answer at the gate what it answers there:
// `default()` falls back instead of failing on `traits` (a 500), and `has()` is true. A
// false assert still refuses, so the gate is not passing vacuously.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in servicevars'
// IncarnationContext.celMap, set `traits` only `if c.Traits != nil`. The first two
// scenarios go red.
func TestPreflightAssert_CreateWithoutTraitsSeesAnEmptyTraitsMap(t *testing.T) {
	t.Setenv("SOUL_STACK_ALLOW_FILE_REPOS", "1")
	gitURL := traitsServiceRepo(t)
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	// No DB: preflightIncarnation answers "no row", the create path.
	r := &Runner{
		deps: Deps{
			Loader:      artifact.NewServiceLoader(t.TempDir(), slog.New(slog.DiscardHandler)),
			ServiceVars: servicevars.NewResolver(nil),
			Render:      render.NewPipeline(nil, engine, nil, nil),
		},
		logger: slog.New(slog.DiscardHandler),
	}
	preflight := func(scenario string) error {
		return r.PreflightAssert(context.Background(), RunSpec{
			IncarnationName: "fresh",
			ServiceRef:      artifact.ServiceRef{Name: "traits-guard", Git: gitURL, Ref: "master"},
			ScenarioName:    scenario,
		})
	}
	for _, scenario := range []string{"team_defaults", "has_traits"} {
		if err := preflight(scenario); err != nil {
			t.Errorf("%s: PreflightAssert = %v, want the assert to pass on an empty traits map", scenario, err)
		}
	}
	if err := preflight("team_is_ops"); !errors.Is(err, ErrAssertFailed) {
		t.Errorf("team_is_ops: PreflightAssert = %v, want ErrAssertFailed", err)
	}
}
