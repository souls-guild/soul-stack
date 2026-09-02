package render

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// resolveTargets resolves a task's hosts: `on:` (Coven-label selection) →
// `where:` (per-host predicate). Returns TargetSIDs sorted by SID and the host
// objects themselves for the subsequent per-host CEL render of params.
//
// `on:` resolution (orchestration.md §3, [ADR-040] amendment 2026-05-27;
// ADR-008 amendment 2026-07-17/NIM-124):
//   - omitted (task.On == nil) → the whole incarnation (all members — the roster
//     is already membership-scoped);
//   - `on: keeper` → keeper-side task, outside pilot scope here → [ErrUnsupportedDSL];
//   - `on: [coven, …]` → AND-intersection over stable Coven labels (a host
//     matches only if it has ALL listed labels), always ⊆ members. A resolved
//     element equal to the incarnation's own name is a validation error
//     (`incarnation.name` is NOT a Coven — omit `on:` to target the whole
//     incarnation; fail-closed so a stale scenario errors out instead of
//     resolving to an empty set).
//
// `where:` resolution — per-host bool predicate (evalWhere). Empty where →
// all targeted hosts.
func resolveTargets(engine *cel.Engine, in RenderInput, task config.Task) ([]*topology.HostFacts, error) {
	covens, err := resolveOn(engine, in, task.On)
	if err != nil {
		return nil, err
	}

	targeted := filterByCovens(in.Hosts, covens)

	if task.Where != "" {
		out := make([]*topology.HostFacts, 0, len(targeted))
		for _, h := range targeted {
			vars := hostVars(in, h, len(targeted))
			vars, err = resolveTaskVars(engine, fileVarsForHost(in, h), task.Vars, vars)
			if err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
			ok, err := evalWhere(engine, task.Where, vars)
			if err != nil {
				return nil, fmt.Errorf("render: task %q (host %s): %w", task.Name, h.SID, err)
			}
			if ok {
				out = append(out, h)
			}
		}
		targeted = out
	}

	// Sort by SID — the "first by SID" contract (golden path) relied on by:
	// renderTaskIter (hi==0 puts render_context/flow_context of the first-by-SID
	// host into rt.Params), compute apply.input (resolves on targeted[0]),
	// run_once: (applyRunOnce takes the first by SID). filterByCovens/where
	// preserve roster order (in.Hosts) — without this sort targeted[0] would be
	// the roster's first host rather than the first by SID, and per-host
	// render_context materialization (RenderContextBySID, keyed by SID) would
	// diverge from what ends up in rt.Params. sidsOf/DispatchPlan.TargetSIDs
	// already sort independently — plan order is unaffected.
	sortBySID(targeted)
	return targeted, nil
}

// sortBySID orders hosts lexicographically by SID in place (determinism for
// "first by SID" — the resolveTargets/renderTaskIter golden path). Idempotent
// on an already-sorted or empty/single-element slice.
func sortBySID(hosts []*topology.HostFacts) {
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].SID < hosts[j].SID })
}

// keeperOnLiteral is the scalar form of `on:` marking a keeper-side task
// (docs/keeper/modules.md). Matches [KeeperTargetSID] by design: the literal
// `on: keeper` and the keeper instance's synthetic target SID denote the same concept.
//
// Since NIM-747 it is no longer how a CORE task says so — the module's address
// is (see [IsKeeperTask]) — and the linter refuses it there as redundant. It
// remains the only spelling a keeper-side PLUGIN address has.
const keeperOnLiteral = "keeper"

// IsKeeperTask reports whether a task executes on the keeper. A keeper-side task
// renders in the keeper context (no per-host roster, see renderKeeperTask) and
// executes locally on the keeper instance via the scenario-runner.
//
// The side follows from the MODULE, not from the task (NIM-747): the core module
// sets are disjoint — `core.state`/`core.cloud`/`core.soul`/`core.vault`/
// `core.choir`/`core.bootstrap`/`core.cert` on this side, the other twenty-one on
// the Soul side — so the address alone decides, and an author restating it in
// `on:` was telling the engine what it already knew. `on:` is back to its one
// meaning, "which covens".
//
// Delegates to [config.IsKeeperSideTask] so this routing and soul-lint's offline
// judgement come out of one rule and one catalog. The legacy `on: keeper`
// literal still routes a PLUGIN address here, which is the half NIM-688 has to
// close before it can go.
func IsKeeperTask(task config.Task) bool {
	return config.IsKeeperSideTask(task)
}

// IsAssertTask reports whether a task is an assert check (ADR-009 amendment
// 2026-06-23): discriminator `assert:`. assert is evaluated Keeper-side during
// the render phase as a run-level precondition and does NOT emit a
// RenderedTask — so it's diverted out of the main Render loop before
// guard/static-when processing (see evalAssertTask).
func IsAssertTask(task config.Task) bool {
	return task.Assert != nil
}

// resolveOn converts an `on:` value into a list of stable Coven labels. A
// returned nil/empty means "no coven filter" (the whole incarnation, only when
// on: is omitted — the roster is already membership-scoped). A resolved element
// equal to the incarnation name is rejected by [resolveCovenList]
// (`incarnation.name` is not a Coven, ADR-008 amendment 2026-07-17/NIM-124).
//
// `on: keeper` does not reach here from a TOP-LEVEL task: [IsKeeperTask] returns
// true for the literal whatever the address, so the pipeline has already diverted
// such a task into renderKeeperTask before roster resolution.
//
// It is still reachable from a block CHILD, which renderBlockTask fans out
// through this same resolve — which is exactly why the config validator refuses a
// keeper-side task inside a block at both levels (`block_on_keeper_invalid`,
// including the one that arrives through an `include:`). So this branch is the
// backstop for a file that got past that check, and is deliberately not deleted.
func resolveOn(engine *cel.Engine, in RenderInput, on any) ([]string, error) {
	switch v := on.(type) {
	case nil:
		return nil, nil
	case string:
		if v == keeperOnLiteral {
			return nil, fmt.Errorf("render: on: keeper reached the Soul-side roster resolve -- a keeper-side task must be routed to renderKeeperTask (programming error)")
		}
		return nil, fmt.Errorf("render: on: %q -- invalid scalar form (expected 'keeper' or a list of covens)", v)
	case []any:
		return resolveCovenList(engine, in, v)
	default:
		return nil, fmt.Errorf("render: on: has type %T, expected string 'keeper' or a list of covens", on)
	}
}

// keeperVars builds the CEL context for rendering a keeper-side task's params:
// exactly the "soulprint-free context" that [resolveCovenList] uses for
// `on:` labels (per-run, not per-host). A keeper task has no hosts →
// soulprint.self/.hosts are unavailable (referencing them in a keeper task's
// params is a normal CEL no-such-key error, as intended: a keeper step
// operates on input/incarnation/vars, not host facts).
//
// compute: IS available (NIM-619). It used to be omitted, which made
// `compute.<name>` here fail as `no such key: <name>` — a sentence about a key
// that was spelled correctly and had just been computed, because the activation
// substitutes an empty map for a namespace the context left out. Both halves of
// that are gone: the namespace is passed through, and the omission itself is no
// longer expressible in silence (compute_scope_guard_test.go).
//
// incarnation.state — read-only pre-run snapshot (RenderInput.State, the same
// stateBefore under FOR UPDATE, see [incarnationVars]): a keeper task
// (core.cloud.destroyed etc.) reads `incarnation.state.<path>` in params just
// like Soul-side. The snapshot is invariant (fixed once, not accumulated
// across passages). nil State → the `state` key isn't set:
// `incarnation.state.<x>` gives a normal no-such-key (push/trial without
// State, backward-compat). The keeper↔soul boundary holds: state is
// operator-facts (not secrets), soulprint.self/.hosts remain unavailable (no hosts).
//
// register: keeper→keeper chaining (staged render, ADR-056) — a keeper task on
// the active Passage sees `register.<prev>.*` from keeper tasks of earlier
// Passages via the [RenderInput.KeeperRegister] channel (the stage loop carries
// it over into keeperRegisterBucket). A keeper task reads ONLY that channel and
// never the per-host buckets. Empty (P0, N=1, non-staged, host-only Passage) →
// falls back to flat Register (backward-compat: trial/push/other callers that
// only set Register see register the same way, bit-for-bit).
//
// The reverse direction is no longer closed: since [ADR-0083] §5 a HOST task
// also reads the keeper bucket, as a union in which its own bucket wins
// ([hostRegister]). The channel stays one-way for the `register.<name>` root —
// host register never leaks into it.
//
// register.hosts.<name> is the DECLARED exception (NIM-711, amendment to
// [ADR-0084]): the per-host buckets inverted by name ([registerHosts]), readable
// only here. A keeper task is the only place `incarnation.state` is written and
// the only place that can see all hosts at once, so a per-host value reaches
// state through one capture reading the whole SID-keyed map. It is a separate
// root, not a widening of `register.<name>`: a keeper task's own chaining
// semantics are unchanged, and every other context ([hostVars], the destiny
// pass, flow-control, migration) is cut off at compile time
// ([cel.Vars.AllowRegisterHosts]).
//
// compute — [Pipeline.resolveCompute] runs once per run, BEFORE the task loop,
// in exactly this soulprint-free run-level context, so `compute.<name>` is the
// same value a Soul-side task sees and nothing about it is per-host. Omitting it
// here made an author's `compute.x` in a keeper task's params fail at eval with
// a bare `no such key: x` while soul-lint accepted the file. [ADR-0083] §4 needs
// it directly: the mint task derives the account set from the same compute the
// scenario's state captures, so the two cannot drift.
func keeperVars(in RenderInput) cel.Vars {
	inc := map[string]any{
		"name":            in.Incarnation.Name,
		"service":         in.Incarnation.Service,
		"service_version": in.Incarnation.ServiceVersion,
		"host_count":      0,
	}
	if in.State != nil {
		inc["state"] = in.State
	}
	reg := in.Register
	if len(in.KeeperRegister) > 0 {
		reg = in.KeeperRegister
	}
	return cel.Vars{
		Input:              in.Input,
		Register:           reg,
		RegisterHosts:      registerHosts(in),
		AllowRegisterHosts: true,
		Incarnation:        inc,
		Vars:               in.ServiceVars,
		Compute:            in.Compute,
		Ctx:                in.Ctx,
		// compute: is in scope here (NIM-619 variant B). It fits this context by
		// construction: [Pipeline.resolveCompute] runs ONCE per run before the task
		// loop, in a soulprint-free run-level context — the same one a keeper task
		// renders in. Nothing about it is per-host, so there is no drift to import.
		ComputeScope: cel.ComputeAvailable,
	}
}

// resolveCovenList computes `on: [...]` elements: static kebab labels as-is;
// CEL wrappers `${ … }` via interpolation (soulprint-free context: `on:`
// resolves once per run, not per host). A resolved element equal to the
// incarnation's own name is a validation error (ADR-008 amendment
// 2026-07-17/NIM-124: `incarnation.name` is not a Coven — the whole-incarnation
// form is an omitted `on:`). Fail-closed: a stale `on: ["${ incarnation.name }"]`
// errors out instead of silently resolving to an empty set.
func resolveCovenList(engine *cel.Engine, in RenderInput, items []any) ([]string, error) {
	// on: resolves not per-host — soulprint is unavailable in this context, and so
	// is compute (declared, so a reference says which, NIM-619).
	vars := cel.Vars{
		Input:    in.Input,
		Register: in.Register,
		Incarnation: map[string]any{
			"name":            in.Incarnation.Name,
			"service":         in.Incarnation.Service,
			"service_version": in.Incarnation.ServiceVersion,
		},
		Ctx:          in.Ctx,
		ComputeScope: cel.ComputeOutOfScopeCovenList,
	}

	out := make([]string, 0, len(items))
	for i, raw := range items {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] has type %T, expected a coven string", i, raw)
		}
		val, err := engine.EvalInterpolation(s, vars)
		if err != nil {
			return nil, fmt.Errorf("render: on[%d] %q: %w", i, s, err)
		}
		coven, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] %q evaluated to %T, expected a coven string", i, s, val)
		}
		// incarnation.name is not a Coven (ADR-008 amendment 2026-07-17/NIM-124):
		// targeting the whole incarnation is an omitted on:, not on: [name].
		if coven == in.Incarnation.Name {
			return nil, fmt.Errorf("render: on[%d] %q resolves to the incarnation name %q, which is not a Coven; omit on: to target the whole incarnation", i, s, in.Incarnation.Name)
		}
		out = append(out, coven)
	}
	return out, nil
}

// filterByCovens keeps hosts that carry ALL covens labels — AND-intersection
// (orchestration.md §3; [ADR-040] amendment 2026-05-27 "multi-label semantics
// within one list"). Empty covens → roster unchanged. Mirrors
// [topology.Resolver.FilterByCovens] as a pure function with no dependency on
// *Resolver (the pipeline doesn't hold one).
//
// Security invariant: AND semantics is fail-closed — listing more labels never
// widens scope.
func filterByCovens(hosts []*topology.HostFacts, covens []string) []*topology.HostFacts {
	if len(covens) == 0 {
		return hosts
	}
	out := make([]*topology.HostFacts, 0, len(hosts))
	for _, h := range hosts {
		if hostHasAllCovens(h.Coven, covens) {
			out = append(out, h)
		}
	}
	return out
}

// hostHasAllCovens is an AND predicate: every required label is present in
// hostCoven. Linear scan (mirrors the counterpart in topology) is faster than
// a map index at typical sizes.
func hostHasAllCovens(hostCoven, required []string) bool {
	for _, want := range required {
		found := false
		for _, c := range hostCoven {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// sidsOf extracts hosts' SIDs, sorted (determinism for DispatchPlan).
func sidsOf(hosts []*topology.HostFacts) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.SID
	}
	sort.Strings(out)
	return out
}

// applyRunOnce implements `run_once: true` (orchestration.md §2.2.2): the step
// runs on exactly ONE host — the first by SID from the on:+where: resolve. >1
// host in the target is normal (deterministically take the first). 0 hosts →
// leave the empty target as-is (run_once introduces no policy of its own for
// an empty target, §5).
//
// run_once==false → target unchanged.
func applyRunOnce(targeted []*topology.HostFacts, runOnce bool) []*topology.HostFacts {
	if !runOnce || len(targeted) <= 1 {
		return targeted
	}
	first := targeted[0]
	for _, h := range targeted[1:] {
		if h.SID < first.SID {
			first = h
		}
	}
	return []*topology.HostFacts{first}
}

// serialWidth computes the `serial:` wave width (orchestration.md §2.2.1) from
// a `serial:` value (int >= 1 or percent-string "<N>%") against the number of
// targeted hosts n. Returns:
//   - 0 — serial: not set (nil); the whole target is one wave.
//   - >=1 — host count per wave (≤ n): for percent, rounded up, minimum 1;
//     for an int, N itself (the dispatcher clamps to ≤ n).
//
// The config validator (validateSerialField) already guaranteed the value's
// shape (int >= 1 or "<N>%", N=1..99), so this is pure computation without
// re-validation; an unrecognized shape → 0 (treated as "not set", fail-safe —
// don't split).
func serialWidth(serial any, n int) int {
	switch v := serial.(type) {
	case nil:
		return 0
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case string:
		return percentWidth(v, n)
	default:
		return 0
	}
}

// percentWidth converts a percent form "<N>%" into a wave host count:
// ceil(n*N/100), minimum 1. Parsing goes through the single
// config.ParseSerialPercent (same source of truth as the config validator).
// An invalid form (shouldn't reach here after the config validator) → 0.
func percentWidth(s string, n int) int {
	pct, ok := config.ParseSerialPercent(s)
	if !ok {
		return 0
	}
	w := (n*pct + 99) / 100 // ceil(n*pct/100)
	if w < 1 {
		w = 1
	}
	return w
}
