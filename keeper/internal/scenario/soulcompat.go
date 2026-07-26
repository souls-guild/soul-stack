package scenario

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
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

// withRunCapability adds a run-level capability (one that follows from the
// ApplyRequest rather than from any single task, e.g. dry_run) to every listed
// SID, creating entries for hosts the plan itself asks nothing of. Returns a new
// map; req is not mutated.
func withRunCapability(req map[string][]string, sids []string, capability string) map[string][]string {
	merged := make(map[string]map[string]struct{}, len(req)+len(sids))
	for sid, caps := range req {
		set := make(map[string]struct{}, len(caps)+1)
		for _, c := range caps {
			set[c] = struct{}{}
		}
		merged[sid] = set
	}
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		if _, ok := merged[sid]; !ok {
			merged[sid] = make(map[string]struct{}, 1)
		}
		merged[sid][capability] = struct{}{}
	}
	return sortedCapabilitySets(merged)
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
		parts = append(parts, fmt.Sprintf("host %s: update the soul binary, it does not announce %s", sid, strings.Join(missing[sid], ", ")))
	}
	return fmt.Errorf(
		"scenario %s/%s: the plan uses modules/features these hosts did not announce, so applying it would be silently ignored there - %s (ADR-0076(i))",
		incarnationName, scenarioName, strings.Join(parts, "; "))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
