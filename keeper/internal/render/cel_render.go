package render

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
)

// renderParams runs the CEL-render phase for one task on one host ([ADR-010]).
// Recursively walks params, evaluating `${ … }` interpolation via cel.Engine in
// every string cell. Non-string values pass through untouched.
//
// Per-host: vars.SoulprintSelf holds facts for this specific host, so the same
// task can render different params on different hosts (e.g. `${
// soulprint.self.os.family }`). Caller invokes renderParams per targeted host
// (see dispatch).
//
// Result is *structpb.Struct (direct fit for proto RenderedTask.params).
// cel.Engine errors ([ErrCompile]/[ErrEval]/[ErrUnsupported]) are wrapped with
// key context.
func renderParams(engine *cel.Engine, params map[string]any, vars cel.Vars) (*structpb.Struct, error) {
	rendered, err := renderValue(engine, params, vars, "")
	if err != nil {
		return nil, err
	}
	m, _ := rendered.(map[string]any)
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("render: params → structpb: %w", err)
	}
	return st, nil
}

// renderValue recursively renders an arbitrary YAML value. path is a
// human-readable cell path for diagnostics (e.g. `acl` or `users[0].name`).
func renderValue(engine *cel.Engine, v any, vars cel.Vars, path string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			rv, err := renderValue(engine, val, vars, joinKey(path, k))
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			rv, err := renderValue(engine, val, vars, joinIdx(path, i))
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case string:
		res, err := engine.EvalInterpolation(t, vars)
		if err != nil {
			return nil, fmt.Errorf("render: ячейка %q: %w", path, err)
		}
		return res, nil
	default:
		return v, nil
	}
}

// evalBoolExpr evaluates a top-level bool predicate ([ADR-010]: whole string =
// CEL) and coerces the result to bool. kind is a human-readable key label
// ("where"/"loop.when") for error messages. Empty expr → true (no predicate).
// Non-bool result → error: predicate must return a boolean.
func evalBoolExpr(engine *cel.Engine, kind, expr string, vars cel.Vars) (bool, error) {
	if expr == "" {
		return true, nil
	}
	out, err := engine.EvalExpression(expr, vars)
	if err != nil {
		return false, fmt.Errorf("render: %s %q: %w", kind, expr, err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("render: %s %q вернул %T, ожидался bool", kind, expr, out.Value())
	}
	return b, nil
}

// evalWhere evaluates the per-host `where:` predicate (orchestration.md §4).
func evalWhere(engine *cel.Engine, where string, vars cel.Vars) (bool, error) {
	return evalBoolExpr(engine, "where", where, vars)
}

// resolveTaskVars builds the final `vars.*` layer for one task on one host:
// BASE is resolved destiny locals `vars.yml` (fileVars), OVERLAID with
// task-level `vars:` (taskVars). Variant A (vars.md): a same-named task-var
// shadows a file-var. In the scenario pass fileVars is empty (vars.yml is a
// destiny concept), so scenario-task behavior is bit-for-bit unchanged. Both
// empty → base returned unchanged.
//
// CRITICAL (cross-layer isolation): taskVars are resolved by resolveVarLayer
// with an EMPTY base.Vars at start — a task-var sees ONLY task-vars in its own
// layer (var→var within layer, eager-topological), never file-vars. file-vars
// are deliberately NOT put into base.Vars before task-vars resolve: otherwise
// `${ vars.<file_var> }` inside a task-var would resolve instead of raising
// ErrVarUnknownRef, breaking the boundary. task-var→task-var cycle →
// ErrVarCycle (mirrors resolveDestinyVars).
func resolveTaskVars(engine *cel.Engine, fileVars, taskVars map[string]any, base cel.Vars) (cel.Vars, error) {
	if len(fileVars) == 0 && len(taskVars) == 0 {
		return base, nil
	}
	resolvedTask, err := resolveVarLayer(engine, taskVars, base)
	if err != nil {
		return cel.Vars{}, err
	}
	resolved := make(map[string]any, len(fileVars)+len(resolvedTask))
	for key, val := range fileVars { // base: file-vars already resolved (no CEL)
		resolved[key] = val
	}
	for key, val := range resolvedTask { // override: task-var shadows file-var
		resolved[key] = val
	}
	base.Vars = resolved
	return base, nil
}

// fileVarsForHost returns resolved destiny locals `vars.yml` for host (base of
// the `vars.*` layer, Variant A). Source is in.DestinyVarsResolved, filled
// per-host by renderApplyDestiny. nil host (synthetic empty context) → key "".
// nil map (scenario pass / destiny without vars.yml) → nil (empty base).
func fileVarsForHost(in RenderInput, host *topology.HostFacts) map[string]any {
	if in.DestinyVarsResolved == nil {
		return nil
	}
	sid := ""
	if host != nil {
		sid = host.SID
	}
	return in.DestinyVarsResolved[sid]
}

// incarnationVars builds the incarnation map for the CEL context: name/service/
// service_version from IncarnationMeta + host_count (number of targeted hosts,
// used by scenario predicates — add_user/main.yml).
//
// state is a read-only snapshot of incarnation.state at the run's row-lock
// capture ([ADR-009]/[ADR-010]): scenario render context sees pre-run state as
// `incarnation.state.<path>` in params/where/apply-input AND in state_changes
// (stateChangesVars calls the same function). The snapshot is INVARIANT across
// all staged-render passages (RenderInput.State is captured once as
// stateBefore under FOR UPDATE, not accumulated between passages — unlike
// register). nil State → key omitted: `incarnation.state.<x>` yields a normal
// no-such-key (push/trial without State, backward-compat), not a compile error
// (`incarnation` is DynType).
func incarnationVars(in RenderInput, hostCount int) map[string]any {
	m := map[string]any{
		"name":            in.Incarnation.Name,
		"service":         in.Incarnation.Service,
		"service_version": in.Incarnation.ServiceVersion,
		"host_count":      hostCount,
	}
	if in.State != nil {
		m["state"] = in.State
	}
	return m
}

// hostVars builds cel.Vars for a specific host: the common run context
// (input/register/incarnation/essence) + soulprint.self for this host.
// Essence is host-invariant (incarnation's effective layer) but is placed in
// every per-host context — available wherever input is.
//
// soulprint.hosts (+ .where) is projected from in.Hosts ONLY in the scenario
// pass (in.destinyIsolated==false). In the destiny pass the host accessor is
// cut off: AllowHosts=false → referencing soulprint.hosts is a compile-time
// isolation error.
func hostVars(in RenderInput, host *topology.HostFacts, hostCount int) cel.Vars {
	return cel.Vars{
		Input:          in.Input,
		Register:       hostRegister(in, host),
		Incarnation:    incarnationVars(in, hostCount),
		SoulprintSelf:  soulprintSelfMap(host),
		SoulprintHosts: soulprintHosts(in),
		Essence:        in.Essence,
		Compute:        in.Compute,
		Ctx:            in.Ctx,
		AllowHosts:     !in.destinyIsolated,
	}
}

// hostRegister selects the register context for CEL-rendering a specific
// host's tasks. Staged render (ADR-056 §c.1): rendering Passage N substitutes
// the PREVIOUS Passages' per-host register — `register.<probe>.*` in
// `where:`/`apply:input:`/`params:`/`vars:` resolves to the fact this host
// collected (a role probe returned 'master' on one host, 'slave' on another).
// Source is in.RegisterByHost[sid] (accumulated by previous Passage barriers,
// threaded through by run.go's stage loop).
//
// Backward-compat: if the per-host map is empty for this host (first Passage,
// N=1 run, or a non-staged path), flat in.Register is returned (empty in the
// pilot). So an N=1 run sees register=empty exactly as before staged-render
// (bit-for-bit), and keeper-side/destiny passes (their own register contexts)
// are unaffected.
func hostRegister(in RenderInput, host *topology.HostFacts) map[string]any {
	if host != nil {
		if reg := in.RegisterByHost[host.SID]; len(reg) > 0 {
			return reg
		}
	}
	return in.Register
}

// buildRenderContext builds the per-host root of the text/template context for
// the core.file.rendered step (templating.md §3.2): `{ vars, self, role,
// essence }` + CONDITIONALLY `input`. Soul passes it as the ROOT to
// text/template (rendered.go).
//
//   - self is the same soulprintSelfMap as in the CEL phase (ADR-018:
//     soulprint.self.<p> in CEL ≡ .self.<p> in the template). role is the
//     host's declared role (may be "").
//   - vars is file-vars (base, fileVars) + the step's params.vars (override) —
//     mirrors resolveTaskVars in the CEL phase (Variant A, vars.md), nested
//     under key vars, not flattened. fileVars is scoped (referencedFileVars:
//     only keys the template reads as `.vars.<key>`) — a template without
//     `.vars.<file_var>` gets no extras, so its `.vars` stays bit-for-bit as
//     before the feature. scenario pass → fileVars empty.
//   - input is the pass's operator-input (Variant B, ADR-010 §3.2): placed
//     ONLY when injectInput (the template actually reads `.input.*`, detected
//     via AST). vars-only templates get no input → their render_context stays
//     bit-for-bit as before Variant B. injectInput && nil Input → empty map
//     (`.input.*` fails strict-mode, which is correct).
//
// Security (seal S-1, ADR-010 §7.4): without vars passthrough, raw
// `${ input.secret }` no longer reaches params → collectSealed never sees it.
// Provenance is reconstructed by the caller (renderTaskIter →
// sealRenderContextInput) DECLARATIVELY from the schema, under the same
// injectInput gate.
func buildRenderContext(in RenderInput, host *topology.HostFacts, fileVars, paramsVars map[string]any, injectInput bool) map[string]any {
	// orEmptyMap normalizes nil (mergeVars returns nil when both are empty) —
	// `.vars` is always present as a key, so `.vars.*` fails meaningfully, not panics.
	rc := map[string]any{
		"vars":    orEmptyMap(mergeVars(fileVars, paramsVars)),
		"self":    soulprintSelfMap(host),
		"role":    host.Role,
		"essence": in.Essence,
	}
	if injectInput {
		rc["input"] = orEmptyMap(in.Input)
	}
	return rc
}

// flowContextSelfKey is the key of flow_context's host-variant section
// (per-host soulprint.self). Excluded from the host-invariant flow_context
// check (flowContextHostInvariant): self is inherently host-variant and is
// covered by a separate regex guard on the predicate text, not snapshot
// comparison.
const flowContextSelfKey = "self"

// buildFlowContext builds a literal per-host snapshot of the non-register part
// of the CEL context for flow-control predicates (when:/changed_when:/
// failed_when:, ADR-012(d)): `{ input, vars, essence, incarnation, self }`.
// This is exactly the context available when rendering this host's params
// (vars cel.Vars), MINUS soulprint.hosts (cross-host, scenario-only — Soul
// doesn't have it) and loop (loop variables aren't placed in flow_context;
// their semantics are render-time fan-out, not a runtime predicate). `self` =
// soulprintSelfMap(host), the same projection as soulprint.self in the CEL
// phase. register.* is NOT placed in flow_context — Soul builds it itself from
// previous tasks' results.
//
// vars is task-level `vars:`, already CEL-resolved (vars.Vars); nil → empty
// map. MVP: the context is FULL, no static pruning (Soul gets the whole
// snapshot even if the predicate references only part of it). Returns
// *structpb.Struct (direct fit for proto RenderedTask.flow_context).
func buildFlowContext(in RenderInput, host *topology.HostFacts, vars cel.Vars, hostCount int) (*structpb.Struct, error) {
	fc := map[string]any{
		"input":            orEmptyMap(vars.Input),
		"vars":             orEmptyMap(vars.Vars),
		"essence":          orEmptyMap(vars.Essence),
		"incarnation":      incarnationVars(in, hostCount),
		flowContextSelfKey: soulprintSelfMap(host),
	}
	st, err := structpb.NewStruct(fc)
	if err != nil {
		return nil, fmt.Errorf("flow_context → structpb: %w", err)
	}
	return st, nil
}

// orEmptyMap turns a nil map into empty (structpb.NewStruct doesn't accept nil
// nesting uniformly; an empty map yields a normal no-such-key on Soul, not a
// panic). Local duplicate of unexported shared/cel.orEmpty — not worth
// exporting for one caller (narrow helper, different packages).
func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// soulprintSelfMap builds soulprint.self for a host: merges reported facts
// (os/network/kernel/cpu/memory, when Soul sent them) with authoritative
// roster registry data (HostFacts).
//
// sid/covens are a Keeper-registry projection (ADR-018, soulprint.md
// "Soulprint ↔ souls-registry boundary"): source of truth is the registry, not
// Soul. So they are ALWAYS placed (even with NULL reported facts: sid's
// authority is the mTLS peer cert, not a collected fact) and OVERWRITE
// same-named reported keys if any collide. role is the declared role from spec
// (may be ""). choirs are the host's Choir names (ADR-044, S-T4); a registry
// projection like covens. traits are operator-set key-value labels (ADR-060);
// a registry projection like covens/choirs.
//
// Symmetric with hostFactsToMap (soulprint.hosts): self and a hosts element
// give consistent sid/covens/role/choirs/traits. host.Soulprint is not
// mutated — a new top-level map is built (reported subsection values are
// shared read-only; render doesn't change them).
func soulprintSelfMap(host *topology.HostFacts) map[string]any {
	self := make(map[string]any, len(host.Soulprint)+5)
	for k, v := range host.Soulprint {
		self[k] = v
	}
	self["sid"] = host.SID
	self["covens"] = covensList(host.Coven)
	self["role"] = host.Role
	self["choirs"] = covensList(host.Choirs)
	// traits are operator-set key-value labels (ADR-060), a registry projection
	// like covens/choirs (overrides a same-named reported key). Always placed
	// (empty map if nil): `soulprint.self.traits.<key>` yields a normal
	// no-such-key, not a missing traits itself.
	self["traits"] = orEmptyMap(host.Traits)
	return self
}

// covensList copies a registry string list (covens/choirs) into []any (cel
// reads a list as []any), without sharing the backing array with the roster.
func covensList(items []string) []any {
	out := make([]any, len(items))
	for i, c := range items {
		out[i] = c
	}
	return out
}

// soulprintHosts projects in.Hosts into []map for the cel accessor
// soulprint.hosts: the stable layer (sid/role/covens/choirs/network/os,
// orchestration.md §4.1). network/os come from the host's last-reported
// Soulprint map; covens/role/choirs are registry data (HostFacts.Coven/Role/
// Choirs). Not projected in the destiny pass (nil) — the accessor is cut off
// there by isolation.
func soulprintHosts(in RenderInput) []map[string]any {
	if in.destinyIsolated || len(in.Hosts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(in.Hosts))
	for _, h := range in.Hosts {
		out = append(out, hostFactsToMap(h))
	}
	return out
}

// hostFactsToMap builds a soulprint.hosts element from HostFacts: stable
// fields. covens/choirs are slice copies (cel reads them as list); traits is
// an operator-set key-value map (ADR-060, registry projection); network/os are
// Soulprint submaps (missing → empty map, field access yields a normal
// no-such-key).
func hostFactsToMap(h *topology.HostFacts) map[string]any {
	return map[string]any{
		"sid":     h.SID,
		"role":    h.Role,
		"covens":  covensList(h.Coven),
		"choirs":  covensList(h.Choirs),
		"traits":  orEmptyMap(h.Traits), // operator-set key-value (ADR-060), like covens/choirs
		"network": soulprintSection(h.Soulprint, "network"),
		"os":      soulprintSection(h.Soulprint, "os"),
	}
}

// soulprintSection extracts a subsection (network/os) from a host's Soulprint
// map. Missing/wrong type → empty map (field access → normal no-such-key, not
// a panic).
func soulprintSection(soulprint map[string]any, key string) map[string]any {
	if soulprint == nil {
		return map[string]any{}
	}
	if sec, ok := soulprint[key].(map[string]any); ok {
		return sec
	}
	return map[string]any{}
}

// hostLoopVars is hostVars + the current `loop:` iteration's variables (`<as>`/
// `<index_as>`, destiny/tasks.md §7). loop=nil → equivalent to hostVars (no
// loop variables). Used by renderLoopTask: each iteration renders params in a
// specific host's context with the active loop variable.
func hostLoopVars(in RenderInput, host *topology.HostFacts, hostCount int, loop map[string]any) cel.Vars {
	v := hostVars(in, host, hostCount)
	v.Loop = loop
	return v
}

// stateChangesVars builds cel.Vars for rendering state_changes.sets on host
// (orchestration.md §7.1). Context is input/incarnation/soulprint.self plus
// this host's Register (grammar slice 2): probe-task register data
// accumulated after the barrier and resolved by register name
// (in.RegisterByHost[host.SID]) over the run-level backing of keeper-side task
// register (bucket KeeperTargetSID, host-wins — ADR-056 amendment
// 2026-07-02). nil register for a host with no keeper backing → `register.*`
// in sets yields an eval "no such key" error, same as before.
func stateChangesVars(in RenderInput, host *topology.HostFacts) cel.Vars {
	reg := in.RegisterByHost[host.SID]
	if keeperReg := in.RegisterByHost[KeeperTargetSID]; len(keeperReg) > 0 {
		merged := make(map[string]any, len(keeperReg)+len(reg))
		for k, v := range keeperReg {
			merged[k] = v
		}
		for k, v := range reg { // host-wins on collision
			merged[k] = v
		}
		reg = merged
	}
	return cel.Vars{
		Input:         in.Input,
		Register:      reg,
		Incarnation:   incarnationVars(in, len(in.Hosts)),
		SoulprintSelf: soulprintSelfMap(host),
		Essence:       in.Essence,
		Compute:       in.Compute,
		Ctx:           in.Ctx,
	}
}

// sortedHostsBySID returns a copy of hosts sorted lexicographically by SID
// (determinism for state_changes.sets' last-wins fold, orchestration.md §7.1).
func sortedHostsBySID(hosts []*topology.HostFacts) []*topology.HostFacts {
	out := make([]*topology.HostFacts, len(hosts))
	copy(out, hosts)
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

func joinKey(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func joinIdx(path string, i int) string {
	return fmt.Sprintf("%s[%d]", path, i)
}
