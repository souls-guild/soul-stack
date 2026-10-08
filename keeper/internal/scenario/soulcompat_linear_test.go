package scenario

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The requirement halves of a linear run's capability gates (NIM-946): the whole plan
// before the first step, and each step from what its hosts are sent.

// Each capability is asked of the hosts that need it and attributed to the first step
// that does, whatever order the survey listed them in.
func TestAheadSoulCapabilities_PerHostAndFirstStep(t *testing.T) {
	needs := []render.SoulNeed{
		{Step: 3, Task: "late", Module: "core.exec.run", RenderedPredicate: true, FlowControl: true, TargetSIDs: []string{"host-a", "host-b"}},
		{Step: 1, Task: "early", Module: "core.exec.run", RenderedPredicate: true, FlowControl: true, TargetSIDs: []string{"host-b"}},
		{Step: 2, Task: "loop", Module: "core.pkg.installed", Retry: true, TargetSIDs: []string{"host-a"}},
		{Step: 4, Task: "plugin", Module: "acme.thing.done", TargetSIDs: []string{"host-a"}},
	}
	required, origins := aheadSoulCapabilities(needs)
	want := map[string][]string{
		"host-a": {config.CapabilityFlowControl, config.ModuleCapability("core.exec"), config.ModuleCapability("core.pkg"), config.CapabilityRenderedPredicate, config.CapabilityRetry},
		"host-b": {config.CapabilityFlowControl, config.ModuleCapability("core.exec"), config.CapabilityRenderedPredicate},
	}
	if !reflect.DeepEqual(required, want) {
		t.Errorf("required = %v, want %v", required, want)
	}
	if got := origins["host-b"][config.CapabilityRenderedPredicate]; got != `step 1 "early"` {
		t.Errorf("host-b's rendered_predicate attributed to %q, want step 1 \"early\"", got)
	}
	if got := origins["host-a"][config.CapabilityRenderedPredicate]; got != `step 3 "late"` {
		t.Errorf("host-a's rendered_predicate attributed to %q, want step 3 \"late\"", got)
	}
}

// ★ GUARD. A step asks each host for what reaches it: a grouped child the Keeper
// skips on host-a travels as `when: false` and needs flow_control there alone; on
// host-b the `when:` was decided and is blank, and only the rendered until: needs a
// capability. The author's shared `when:` asks for nothing.
//
// Mutations: read t.When instead of the wire → host-b asks for flow_control; ignore
// HostFlow.Rendered → no rendered_predicate.
func TestStepSoulCapabilities_FromTheWire(t *testing.T) {
	child := &render.RenderedTask{
		Index:      4,
		Name:       "restart",
		Module:     "core.exec.run",
		When:       "register.probe.stdout == 'yes'",
		Until:      "register.self.rc == register.probe.rc",
		RetryCount: 3,
		Grouped:    true,
		FlowBySID: map[string]render.HostFlow{
			"host-a": {Skip: true},
			"host-b": {Until: "register.self.rc == dyn(0)", Rendered: true},
		},
	}
	plain := &render.RenderedTask{Index: 5, Name: "verify", Module: "core.exec.run"}
	required, origins := stepSoulCapabilities(map[string][]*render.RenderedTask{
		"host-a": {child, plain},
		"host-b": {child, plain},
	}, 2)
	want := map[string][]string{
		"host-a": {config.CapabilityFlowControl, config.ModuleCapability("core.exec"), config.CapabilityRetry},
		"host-b": {config.ModuleCapability("core.exec"), config.CapabilityRenderedPredicate, config.CapabilityRetry},
	}
	if !reflect.DeepEqual(required, want) {
		t.Errorf("required = %v, want %v", required, want)
	}
	if got := origins["host-b"][config.CapabilityRenderedPredicate]; got != `step 2 "restart"` {
		t.Errorf("attributed to %q, want step 2 \"restart\"", got)
	}
}

// A push host announces nothing and is not asked (NIM-880); a refusal of a later step
// says where it came, and the run aborts as soul_capability_unsupported, not as a
// dispatch failure.
func TestGateStepCapabilities_PushExemptAndReason(t *testing.T) {
	r := &Runner{soulCap: stubSoulCap{lackingByCap: map[string][]string{config.CapabilityRenderedPredicate: {"host-a", "host-push"}}}}
	rendered := &render.RenderedTask{
		Name:      "check",
		Module:    "core.exec.run",
		FlowBySID: map[string]render.HostFlow{"host-a": {FailedWhen: "x", Rendered: true}, "host-push": {FailedWhen: "x", Rendered: true}},
	}
	hosts := []*topology.HostFacts{{SID: "host-a"}, {SID: "host-push", Transport: config.TransportSSH}}
	spec := RunSpec{IncarnationName: "redis-prod", ScenarioName: "restart"}

	err := r.gateStepCapabilities(context.Background(), spec, 3, map[string][]*render.RenderedTask{"host-push": {rendered}}, hosts)
	if err != nil {
		t.Fatalf("a push host alone must not be gated: %v", err)
	}
	err = r.gateStepCapabilities(context.Background(), spec, 3, map[string][]*render.RenderedTask{"host-a": {rendered}, "host-push": {rendered}}, hosts)
	if err == nil {
		t.Fatal("host-a lacks the bit: the step must be refused")
	}
	if got := dispatchAbortReason(err); got != reasonSoulCapabilityUnsupported {
		t.Errorf("abort reason = %q, want %s", got, reasonSoulCapabilityUnsupported)
	}
	if got := dispatchAbortReason(errors.New("send failed")); got != "dispatch_failed" {
		t.Errorf("abort reason of a send failure = %q, want dispatch_failed", got)
	}
	for _, want := range []string{"host host-a", `rendered_predicate (step 3 "check")`, "refused at step 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "host-push") {
		t.Errorf("error %q names the push host", err)
	}
}
