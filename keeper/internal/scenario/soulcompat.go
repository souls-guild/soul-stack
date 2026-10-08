package scenario

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Soul-side axis of engine compatibility (ADR-0076(i)), the counterpart of the
// keeper-side version window in compat.go. Keeper cannot ask a version number
// whether a binary carries module X — the soul estate is large and
// heterogeneous, so the contract is the capability set each binary announces on
// Hello (ADR-056 §S5, generalized) and keeper's job is to derive what a
// particular run needs from the plan it just rendered.
//
// What this closes is the dangerous failure mode of ADR-0076: an old soul that
// does not implement something the plan uses does NOT fail loudly — it reads the
// params it knows by key and reports OK/CHANGED with no effect. A gate before
// dispatch turns that silence into an honest per-host rejection.

// reasonSoulCapabilityUnsupported is the abort reason for a plan requiring more
// than a target host announced. Sibling of soul_passage_unsupported (same axis,
// same fail-closed shape) and of keeper_version_unsupported (the other axis).
const reasonSoulCapabilityUnsupported = "soul_capability_unsupported"

// requiredSoulCapabilities maps target SID → the capability set the rendered plan
// needs from THAT host, sorted. Attribution is per-host by design: a plan is not
// one requirement but N, and rejecting a whole run because some unrelated host
// runs an older binary would block work that would have succeeded.
//
// Per rendered task:
//   - `module:<name>` for a core module (namespace `core`). A plugin module is
//     deliberately NOT required: it can be installed mid-run by
//     core.module.installed (ADR-065), long after the announcement was made at
//     connect time — gating on one would reject a legitimate install-then-use
//     scenario. The plugin hole stays with param-level strictness (NIM-163).
//   - `flow_control` when the task carries when:/changed_when:/failed_when: —
//     keeper threads these through as CEL strings for the Soul to evaluate
//     (ADR-012(d)).
//   - `retry` when the task carries a retry loop or its until: exit predicate —
//     also enforced Soul-side.
//
// Skipped: keeper-side tasks (`on: keeper`, DispatchPlan.Keeper — they run
// locally on this instance, against keeper's own registry) and any plan with no
// targets, which covers both a statically-skipped task (when: false — never
// executed, so it requires nothing) and a future-Passage placeholder of a staged
// render (targets unknown until its Passage becomes active; it is gated then,
// before ITS dispatch, by the per-Passage call site).
func requiredSoulCapabilities(tasks []*render.RenderedTask, plans []render.DispatchPlan) map[string][]string {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		if t != nil {
			byIndex[t.Index] = t
		}
	}
	req := make(map[string]map[string]struct{})
	for _, p := range plans {
		if p.Keeper || len(p.TargetSIDs) == 0 {
			continue
		}
		t, ok := byIndex[p.TaskIndex]
		if !ok {
			continue
		}
		caps := taskSoulCapabilities(t)
		if len(caps) == 0 {
			continue
		}
		for _, sid := range p.TargetSIDs {
			if sid == "" {
				continue
			}
			set, ok := req[sid]
			if !ok {
				set = make(map[string]struct{}, len(caps))
				req[sid] = set
			}
			for _, c := range caps {
				set[c] = struct{}{}
			}
		}
	}
	return sortedCapabilitySets(req)
}

// taskSoulCapabilities is the per-task half of [requiredSoulCapabilities].
func taskSoulCapabilities(t *render.RenderedTask) []string {
	var caps []string
	if name, _, ok := config.SplitModuleAddr(t.Module); ok && strings.HasPrefix(name, "core.") {
		caps = append(caps, config.ModuleCapability(name))
	}
	if t.When != "" || t.ChangedWhen != "" || t.FailedWhen != "" {
		caps = append(caps, config.CapabilityFlowControl)
	}
	if t.Until != "" || t.RetryCount > 1 {
		caps = append(caps, config.CapabilityRetry)
	}
	return caps
}

// capabilityOrigins names, per host and capability, the first task that needs it —
// `step N "task"` — for the refusal to say what to look at.
type capabilityOrigins map[string]map[string]string

func (o capabilityOrigins) note(sid, capability string, step int, task string) {
	if o[sid] == nil {
		o[sid] = map[string]string{}
	}
	if _, ok := o[sid][capability]; !ok {
		o[sid][capability] = fmt.Sprintf("step %d %q", step, task)
	}
}

// aheadSoulCapabilities is the requirement of a linear run's whole plan, before its
// first step (draft ADR NIM-907 item 10, NIM-946): every task against the hosts its
// targeting resolves to before the run ([render.SoulNeed]). In step order, so a
// capability is attributed to the first step that needs it.
func aheadSoulCapabilities(needs []render.SoulNeed) (map[string][]string, capabilityOrigins) {
	sorted := slices.Clone(needs)
	slices.SortStableFunc(sorted, func(a, b render.SoulNeed) int { return a.Step - b.Step })
	req := make(map[string]map[string]struct{})
	origins := capabilityOrigins{}
	for _, n := range sorted {
		var caps []string
		if name, _, ok := config.SplitModuleAddr(n.Module); ok && strings.HasPrefix(name, "core.") {
			caps = append(caps, config.ModuleCapability(name))
		}
		if n.FlowControl {
			caps = append(caps, config.CapabilityFlowControl)
		}
		if n.Retry {
			caps = append(caps, config.CapabilityRetry)
		}
		if n.RenderedPredicate {
			caps = append(caps, config.CapabilityRenderedPredicate)
		}
		addCapabilities(req, origins, n.TargetSIDs, caps, n.Step, n.Task)
	}
	return sortedCapabilitySets(req), origins
}

// stepSoulCapabilities is the requirement of one step of a linear run, per host,
// from what that host is sent once the Keeper settled the step: after the requisite
// gate and the Keeper's `when:`, so a host is asked only for what reaches it — a
// grouped child the Keeper skips there travels as `when: false` and needs
// flow_control on that host alone.
func stepSoulCapabilities(perHost map[string][]*render.RenderedTask, step int) (map[string][]string, capabilityOrigins) {
	req := make(map[string]map[string]struct{})
	origins := capabilityOrigins{}
	for _, sid := range sortedSIDs(perHost) {
		for _, t := range perHost[sid] {
			addCapabilities(req, origins, []string{sid}, sentSoulCapabilities(t, sid), step, t.Name)
		}
	}
	return sortedCapabilitySets(req), origins
}

// sentSoulCapabilities is [taskSoulCapabilities] over what host sid is sent for t
// ([render.SentFlow]) rather than the author's predicates, plus the rendered
// predicate's bit.
func sentSoulCapabilities(t *render.RenderedTask, sid string) []string {
	var caps []string
	if name, _, ok := config.SplitModuleAddr(t.Module); ok && strings.HasPrefix(name, "core.") {
		caps = append(caps, config.ModuleCapability(name))
	}
	flow := render.SentFlow(t, sid)
	if flow.When != "" || flow.ChangedWhen != "" || flow.FailedWhen != "" {
		caps = append(caps, config.CapabilityFlowControl)
	}
	if flow.Until != "" || t.RetryCount > 1 {
		caps = append(caps, config.CapabilityRetry)
	}
	if flow.Rendered {
		caps = append(caps, config.CapabilityRenderedPredicate)
	}
	return caps
}

func addCapabilities(req map[string]map[string]struct{}, origins capabilityOrigins, sids, caps []string, step int, task string) {
	if len(caps) == 0 {
		return
	}
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		set, ok := req[sid]
		if !ok {
			set = make(map[string]struct{}, len(caps))
			req[sid] = set
		}
		for _, c := range caps {
			set[c] = struct{}{}
			origins.note(sid, c, step, task)
		}
	}
}

func sortedCapabilitySets(in map[string]map[string]struct{}) map[string][]string {
	out := make(map[string][]string, len(in))
	for sid, set := range in {
		caps := make([]string, 0, len(set))
		for c := range set {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		out[sid] = caps
	}
	return out
}

// gateSoulCapabilities rejects the run unless every target host announced what
// the plan needs from it (ADR-0076(i)). Fail-closed on both edges: a nil checker
// (no Redis — no presence source to confirm support against) and a Redis failure
// reject just as a missing capability does. Returns nil when nothing is
// required.
//
// The checker is per-capability, so this issues one batched call per DISTINCT
// capability over the hosts that need it — a plan touching k modules across the
// roster costs k pipelines, not one per host.
func (r *Runner) gateSoulCapabilities(ctx context.Context, incarnationName, scenarioName string, required map[string][]string) error {
	return r.gateSoulCapabilitiesFor(ctx, incarnationName, scenarioName, required, nil)
}

// gateSoulCapabilitiesFor is [Runner.gateSoulCapabilities] whose refusal names, for
// each missing capability, the first task that needs it.
func (r *Runner) gateSoulCapabilitiesFor(ctx context.Context, incarnationName, scenarioName string, required map[string][]string, origins capabilityOrigins) error {
	if len(required) == 0 {
		return nil
	}
	sidsByCap := make(map[string][]string)
	for sid, caps := range required {
		for _, c := range caps {
			sidsByCap[c] = append(sidsByCap[c], sid)
		}
	}
	if r.soulCap == nil {
		return fmt.Errorf(
			"scenario %s/%s: the plan requires soul capabilities %s, but the presence checker is unavailable (no Redis) - cannot confirm host support, fail-closed rejection (ADR-0076(i))",
			incarnationName, scenarioName, sortedKeys(sidsByCap))
	}

	// Missing capabilities are collected per host, so an operator upgrading a
	// fleet sees every host that needs the binary bumped in one message instead
	// of rediscovering the next one on each retry.
	missing := make(map[string][]string)
	for _, capability := range sortedKeys(sidsByCap) {
		sids := sidsByCap[capability]
		sort.Strings(sids)
		lacking, err := r.soulCap.SoulsLackingCapability(ctx, sids, capability)
		if err != nil {
			return fmt.Errorf(
				"scenario %s/%s: host capability check for %q failed - fail-closed rejection (ADR-0076(i)): %w",
				incarnationName, scenarioName, capability, err)
		}
		for _, sid := range lacking {
			missing[sid] = append(missing[sid], capability)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	parts := make([]string, 0, len(missing))
	for _, sid := range sortedKeys(missing) {
		caps := make([]string, 0, len(missing[sid]))
		for _, c := range missing[sid] {
			if origin := origins[sid][c]; origin != "" {
				c += " (" + origin + ")"
			}
			caps = append(caps, c)
		}
		parts = append(parts, fmt.Sprintf("host %s: update the soul binary, it does not announce %s", sid, strings.Join(caps, ", ")))
	}
	return fmt.Errorf(
		"scenario %s/%s: the plan uses modules/features these hosts did not announce, so applying it would be silently ignored or misread there - %s (ADR-0076(i))",
		incarnationName, scenarioName, strings.Join(parts, "; "))
}

// gateLinearPlanCapabilities is the capability gate of a linear run ahead of its
// first step (draft ADR NIM-907 item 10): the whole plan, as far as it is known before
// the run, so a host that cannot run a later step refuses the run before an earlier
// one changed anything. What it cannot know is gated at each step
// ([Runner.gateStepCapabilities]).
func (r *Runner) gateLinearPlanCapabilities(ctx context.Context, spec RunSpec, needs []render.SoulNeed, hosts []*topology.HostFacts) error {
	required, origins := aheadSoulCapabilities(needs)
	required = exemptPushHosts(required, hosts)
	spec.prov.observeRequired(required)
	return r.gateSoulCapabilitiesFor(ctx, spec.IncarnationName, spec.ScenarioName, required, origins)
}

// errStepCapability is a step of a linear run refused by the capability gate; the run
// aborts as reasonSoulCapabilityUnsupported rather than as a dispatch failure.
type errStepCapability struct{ err error }

func (e *errStepCapability) Error() string { return e.err.Error() }
func (e *errStepCapability) Unwrap() error { return e.err }

// gateStepCapabilities gates one step of a linear run on what each host is sent,
// once the Keeper settled the step and before any of its `apply_runs` rows is written
// or anything is sent (its `apply_run_plan` rows are persisted before). It
// refuses late only what [Runner.gateLinearPlanCapabilities] could not know
// (render/soul_needs.go lists it): a task whose targeting reads a register or state an
// earlier capture changes, a host or a task a change of the roster brings in, a destiny
// task the destiny's render could switch off on a host, the `when: false` a grouped
// child is sent where the Keeper skips it, and a task the survey could not judge.
func (r *Runner) gateStepCapabilities(ctx context.Context, spec RunSpec, step int, perHost map[string][]*render.RenderedTask, hosts []*topology.HostFacts) error {
	required, origins := stepSoulCapabilities(perHost, step)
	required = exemptPushHosts(required, hosts)
	spec.prov.observeRequired(required)
	if err := r.gateSoulCapabilitiesFor(ctx, spec.IncarnationName, spec.ScenarioName, required, origins); err != nil {
		if step > 0 {
			err = fmt.Errorf("%w; refused at step %d, after the steps before it ran", err, step)
		}
		return &errStepCapability{err: err}
	}
	return nil
}

func dispatchAbortReason(err error) string {
	var capErr *errStepCapability
	if errors.As(err, &capErr) {
		return reasonSoulCapabilityUnsupported
	}
	return "dispatch_failed"
}

// streamedSIDs lists the roster hosts whose capabilities are a question anyone
// can answer: the ones that opened an EventStream and announced a set on Hello.
//
// A `transport: ssh` host is left out (NIM-880). Nothing about it is announced —
// it holds no lease, sends no Hello, and the binary that will run there is the
// one this Keeper delivers from `push.soul_binary_path` at dispatch time. Asking
// the presence source about it returns "lacking every capability", which would
// reject every run that touched a push host; the honest answer is that this gate
// has no jurisdiction there. What a push host runs is an operator's keeper.yml
// key, and a mismatch surfaces as the run's own failure on that host, not as a
// silent no-op — which is the failure mode this gate exists to prevent.
func streamedSIDs(hosts []*topology.HostFacts) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == nil || h.Transport == config.TransportSSH {
			continue
		}
		out = append(out, h.SID)
	}
	return out
}

// exemptPushHosts drops the push hosts from a per-host capability requirement
// map, for the same reason [streamedSIDs] leaves them out of the roster it asks
// about. Applied to the requirement rather than inside the gate so the
// observability counter (prov.observeRequired) reports what is actually gated.
//
// ⚠ It MUTATES and returns the map it was given. Every call site wraps a fresh
// requirement ([requiredSoulCapabilities], [aheadSoulCapabilities],
// [stepSoulCapabilities]), so nothing is shared today; a caller passing a cached map
// would have that cache stripped for every later reader.
func exemptPushHosts(required map[string][]string, hosts []*topology.HostFacts) map[string][]string {
	if len(required) == 0 {
		return required
	}
	for _, h := range hosts {
		if h != nil && h.Transport == config.TransportSSH {
			delete(required, h.SID)
		}
	}
	return required
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gatePassageCapability is the ADR-056 §S5 forward-compat gate: every STREAMED
// host a staged run will touch must have announced that it echoes
// `ApplyRequest.passage`. A Soul that cannot returns passage=0 for every
// Passage, so the barrier for Passage 1+ waits for a terminal that never
// arrives and the incarnation sits in `applying` until the run timeout.
//
// Fail-closed on both edges: a nil checker (no presence source to confirm
// against) and a check error reject the run exactly as a missing capability
// does. Only a staged run calls it — an N=1 run sends a single passage=0 and is
// compatible with an old binary bit-for-bit.
//
// ★ Push hosts are left out of the question rather than answered wrongly
// (NIM-880): they announce nothing, so asking reports them lacking every
// capability. Their half of this gate is [Runner.dispatchPushHost]'s echo check,
// which tests the delivered binary directly instead of asking about it. A roster
// with NO streamed host therefore has nothing to gate — but an EMPTY roster is
// not that case: a provision-from-zero run has no hosts yet, and the fail-closed
// arms below are precisely what §S5 wants there.
//
// It is called again at EVERY refresh boundary (ADR-0061 §S3), not only when the
// roster grew: the boundary re-resolves a live snapshot, which can also shrink,
// and the delta is not computed. The case that needs it is growth — a run that
// started all-push skips the gate, and the agent host it picks up mid-run would
// otherwise be dispatched to unasked — but the cost on a boundary that lost
// hosts is one `SoulsLackingCapability` round-trip, and a transient failure
// there aborts the run fail-closed like any other §S5 answer.
func (r *Runner) gatePassageCapability(ctx context.Context, spec RunSpec, hosts []*topology.HostFacts, passageCount int) error {
	sids := streamedSIDs(hosts)
	if len(hosts) > 0 && len(sids) == 0 {
		return nil
	}
	// Under linear dispatch every run of more than one step comes here (draft ADR
	// NIM-907 item 8), and removing a register dependency removes no step.
	linear := spec.executionModel == ExecutionLinear
	if r.soulCap == nil {
		if linear {
			return fmt.Errorf(
				"scenario %s/%s: run of %d steps requires confirmation of host passage-capability, but the presence checker is unavailable (no Redis) - fail-closed rejection (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passageCount)
		}
		return fmt.Errorf(
			"scenario %s/%s: staged run (%d Passage) requires confirmation of host passage-capability, but the presence checker is unavailable (no Redis) - fail-closed rejection (ADR-056 §S5)",
			spec.IncarnationName, spec.ScenarioName, passageCount)
	}
	lacking, err := r.soulCap.SoulsLackingCapability(ctx, sids, config.CapabilityPassage)
	if err != nil {
		if linear {
			return fmt.Errorf(
				"scenario %s/%s: run of %d steps: host passage-capability check failed - fail-closed rejection (ADR-056 §S5): %w",
				spec.IncarnationName, spec.ScenarioName, passageCount, err)
		}
		return fmt.Errorf(
			"scenario %s/%s: staged run host passage-capability check failed - fail-closed rejection (ADR-056 §S5): %w",
			spec.IncarnationName, spec.ScenarioName, err)
	}
	if len(lacking) > 0 {
		if linear {
			return fmt.Errorf(
				"scenario %s/%s: run of %d steps requires a Passage-aware Soul, but hosts %v do not support the passage field - update the soul binary (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passageCount, lacking)
		}
		return fmt.Errorf(
			"scenario %s/%s: staged run (%d Passage by register dependency) requires a Passage-aware Soul, but hosts %v do not support the passage field - update the soul binary or remove the register dependency (ADR-056 §S5)",
			spec.IncarnationName, spec.ScenarioName, passageCount, lacking)
	}
	return nil
}
