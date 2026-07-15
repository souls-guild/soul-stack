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
// `on:` resolution (orchestration.md §3, [ADR-040] amendment 2026-05-27):
//   - omitted (task.On == nil) → the whole incarnation (all hosts);
//   - `on: keeper` → keeper-side task, outside pilot scope here → [ErrUnsupportedDSL];
//   - `on: [coven, …]` → AND-intersection over Coven labels (a host matches only
//     if it has ALL listed labels). Coven literals wrapped in CEL `${ … }`
//     (e.g. `${ incarnation.name }`) are evaluated; `${ incarnation.name }`
//     passes through the common filter as an ordinary label and doesn't narrow
//     scope — every host in the roster carries the root label by construction
//     (rosterSQL `WHERE $1 = ANY(coven)`, ADR-008), so filtering on it is
//     equivalent to "the whole incarnation".
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
const keeperOnLiteral = "keeper"

// IsKeeperTask reports whether a task is declared keeper-side (`on: keeper`,
// docs/keeper/modules.md). A keeper-side task renders in the keeper context
// (no per-host roster, see renderKeeperTask) and executes locally on the
// keeper instance via the scenario-runner. Other forms of `on:` (omitted /
// coven list) are Soul-side. The config validator guarantees the scalar form
// of `on:` is only "keeper".
func IsKeeperTask(task config.Task) bool {
	s, ok := task.On.(string)
	return ok && s == keeperOnLiteral
}

// IsAssertTask reports whether a task is an assert check (ADR-009 amendment
// 2026-06-23): discriminator `assert:`. assert is evaluated Keeper-side during
// the render phase as a run-level precondition and does NOT emit a
// RenderedTask — so it's diverted out of the main Render loop before
// guard/static-when processing (see evalAssertTask).
func IsAssertTask(task config.Task) bool {
	return task.Assert != nil
}

// resolveOn converts an `on:` value into a list of Coven labels. A returned
// nil/empty means "no coven filter" (the whole incarnation, only when on: is
// omitted). The root label `${ incarnation.name }` gets NO special handling:
// it enters the list as an ordinary label, and filtering on it is safe and
// equivalent to "the whole incarnation" — every host in the roster carries the
// root label (rosterSQL `WHERE $1 = ANY(coven)`, ADR-008).
//
// `on: keeper` never reaches here — keeper-side tasks are diverted by the
// pipeline into renderKeeperTask before roster resolution (see
// [IsKeeperTask]); this branch is defense-in-depth (a routing bug → an
// explicit error, not silent misbehavior).
func resolveOn(engine *cel.Engine, in RenderInput, on any) ([]string, error) {
	switch v := on.(type) {
	case nil:
		return nil, nil
	case string:
		if v == keeperOnLiteral {
			return nil, fmt.Errorf("render: on: keeper достиг Soul-side резолва roster — keeper-side задача должна маршрутизироваться в renderKeeperTask (программная ошибка)")
		}
		return nil, fmt.Errorf("render: on: %q — недопустимая скалярная форма (ожидалось 'keeper' или список ковенов)", v)
	case []any:
		return resolveCovenList(engine, in, v)
	default:
		return nil, fmt.Errorf("render: on: имеет тип %T, ожидалась строка 'keeper' или список ковенов", on)
	}
}

// keeperVars builds the CEL context for rendering a keeper-side task's params:
// exactly the "soulprint-free context" that [resolveCovenList] uses for
// `on:` labels (per-run, not per-host). A keeper task has no hosts →
// soulprint.self/.hosts are unavailable (referencing them in a keeper task's
// params is a normal CEL no-such-key error, as intended: a keeper step
// operates on input/incarnation/essence, not host facts).
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
// Passages via the ISOLATED [RenderInput.KeeperRegister] channel (the stage
// loop carries it over into keeperRegisterBucket). The channel is
// DELIBERATELY separate from flat Register: the host fallback ([hostRegister])
// stays on Register, so a mixed-Passage host task does NOT read
// keeper-register when the per-host bucket is empty. Empty (P0, N=1,
// non-staged, host-only Passage) → falls back to flat Register
// (backward-compat: trial/push/other callers that only set Register see
// register the same way, bit-for-bit).
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
		Input:       in.Input,
		Register:    reg,
		Incarnation: inc,
		Essence:     in.Essence,
		Ctx:         in.Ctx,
	}
}

// resolveCovenList computes `on: [...]` elements: static kebab labels as-is;
// CEL wrappers `${ … }` via interpolation (soulprint-free context: `on:`
// resolves once per run, not per host). The root label `incarnation.name` gets
// NO special handling — it enters the list like any other: filtering on it is
// safe and equivalent to "the whole incarnation", since every host in the
// roster carries the root label (rosterSQL `WHERE $1 = ANY(coven)`, ADR-008).
func resolveCovenList(engine *cel.Engine, in RenderInput, items []any) ([]string, error) {
	// on: resolves not per-host — soulprint is unavailable in this context.
	vars := cel.Vars{
		Input:    in.Input,
		Register: in.Register,
		Incarnation: map[string]any{
			"name":            in.Incarnation.Name,
			"service":         in.Incarnation.Service,
			"service_version": in.Incarnation.ServiceVersion,
		},
		Ctx: in.Ctx,
	}

	out := make([]string, 0, len(items))
	for i, raw := range items {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] имеет тип %T, ожидалась строка-coven", i, raw)
		}
		val, err := engine.EvalInterpolation(s, vars)
		if err != nil {
			return nil, fmt.Errorf("render: on[%d] %q: %w", i, s, err)
		}
		coven, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("render: on[%d] %q вычислился в %T, ожидалась строка-coven", i, s, val)
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
