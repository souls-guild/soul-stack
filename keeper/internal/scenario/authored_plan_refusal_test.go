package scenario

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ GUARD. The Keeper refuses what soul-lint refuses on the expanded plan: a
// block's retry: over an applier aborts the run before render, under the code
// soul-lint reports (NIM-910).
//
// Mutation: drop config.RetryOverApply from authoredPlanRefusal → the run goes on
// to render, where only guardPilotBlockChild stops it, under render_failed.
func TestAuthoredPlanRefusal_BlockRetryOverApplier(t *testing.T) {
	scn := &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
		Name:  "grp",
		Retry: &config.RetrySpec{Count: 3},
		Block: &config.BlockTask{Block: []config.Task{{Name: "apply-step", Apply: &config.ApplyTask{Destiny: "d"}}}},
	}}}
	d, bad := authoredPlanRefusal(scn)
	if !bad || d.Code != "retry_on_apply_invalid" {
		t.Fatalf("authoredPlanRefusal = (%+v, %v), want retry_on_apply_invalid", d, bad)
	}
}
