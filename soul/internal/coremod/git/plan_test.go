package git_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/git"
)

// TestGit_NotPlanReadSafe — core.git does NOT declare PlanReadSafe in MVP:
// for state `pulled`, the drift check "are there upstream updates?" needs
// `git fetch`, which Apply doesn't run before mutating (see Plan's doc). The
// host applies default-deny (FAILED `plan.unsupported`) on dry_run — a
// separate slice (Slice B) will add support, or split into cloned/pulled state.
func TestGit_NotPlanReadSafe(t *testing.T) {
	m := git.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.git реализует PlanReadSafe (не должен в MVP — см. doc Plan)")
	}
}
