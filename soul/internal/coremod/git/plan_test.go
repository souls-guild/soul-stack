package git_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/git"
)

// TestGit_NotPlanReadSafe — core.git в MVP НЕ объявляет PlanReadSafe:
// для state `pulled` drift «есть ли upstream-обновления?» требует `git fetch`,
// которого Apply ДО мутации не делает (см. doc Plan). Host применит
// default-deny (FAILED `plan.unsupported`) на dry_run — отдельный slice
// (Slice B) добавит поддержку, либо введёт split на cloned/pulled state.
func TestGit_NotPlanReadSafe(t *testing.T) {
	m := git.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.git реализует PlanReadSafe (не должен в MVP — см. doc Plan)")
	}
}
