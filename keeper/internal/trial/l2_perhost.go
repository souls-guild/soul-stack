package trial

import (
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// l2PerHostPlanRefusal reports why an L2 case cannot run, or "" when it can.
//
// L2 applies ONE ApplyRequest to ONE stand, so a plan whose params differ per host
// (NIM-908, [render.RenderedTask.ParamsBySID]) has no host to be converted for.
// Refused rather than flattened: [render.ToProtoTasks] would answer with the first
// host by SID for a fixture that deliberately declares several, and the case would
// pass while proving something about a host it never applied to.
//
// Raised BEFORE the stand starts — there is nothing to container for — and split out
// so it is testable without docker, which the gate does not have.
//
// ★ And L0 does NOT cover it either, which is stated rather than papered over:
// `compareRenderedTasks` asserts `expect.tasks[].params` against `rt.Params`, the
// first host by SID, with no host axis in the case format. So a multi-host per-host
// plan has no trial tier that can assert what host 2 received — the unit tests in
// keeper/internal/render are the only place, and giving L0 a per-host assertion is
// its own ticket.
func l2PerHostPlanRefusal(tasks []*render.RenderedTask) string {
	if !render.PlanIsPerHost(tasks) {
		return ""
	}
	return "L2: the plan renders different params per host, and an L2 stand is one host -- " +
		"drop the extra hosts from the fixture, or assert the per-host plan at L0"
}
