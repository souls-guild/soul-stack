package scenario

import (
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// The linear-dispatch benchmark (dev/bench/linear-dispatch, NIM-950) compares one
// message per host against twelve on the same scenario. If an edit to the fixture
// made ADR-056 stage it into several Passages, or folded tasks into one step, the
// published numbers would compare something else than they claim.
//
// Mutation: give t01 `register: t01` and make t12's params read it → staged count 2.
// (A when: alone does not: ADR-056 evaluates it on the Soul, inside the one message.)
func TestBenchFixture_TwelveStepsOnePassage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "dev", "bench", "linear-dispatch", "service", "scenario", "bench", "main.yml")
	m, _, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load %s: %v %+v", path, err, diags)
	}
	staged, err := render.Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if staged.Count != 1 {
		t.Errorf("staged Passages = %d, want 1", staged.Count)
	}
	if linear, _ := config.LinearPassage(m.Tasks, m.Settings); linear.Count != 12 {
		t.Errorf("linear steps = %d, want 12", linear.Count)
	}
}
