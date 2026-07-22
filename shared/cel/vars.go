package cel

import (
	"context"
	"sort"
)

// Vars — context variables passed into a CEL evaluation. A typed form of activation:
// the caller fills meaningful fields rather than a raw map[string]any. All fields are
// optional; nil becomes an empty map, so accessing a missing context gives the normal
// CEL result (no such key), not a panic.
//
// Field values are plain Go data (map[string]any, slices, scalars) obtained from
// YAML/Postgres. CEL reads them through the cel-go adapter.
//
// Pilot scope ([ADR-010]):
//   - Input        — the input: block of the scenario/destiny (input.<path>).
//   - Register     — results of register: from previous steps
//     (register.<name>.<path>, register.self.*).
//   - Incarnation  — incarnation fields (name, service_version, spec.*).
//   - SoulprintSelf — stable facts of the current host; in CEL available as
//     soulprint.self.<path> ([soulprint.md], canonical form).
//   - SoulprintHosts — the list of run hosts with stable facts; in CEL available
//     as soulprint.hosts (+ .where(<predicate>)). Scenario-only: filled only in the
//     render's scenario pass. nil/empty ⇒ soulprint.hosts is an empty list (and in
//     the destiny pass accessing it is an isolation error, see [Vars.allowHosts]).
//     [orchestration.md §4.1].
//   - Essence       — the effective essence layer (essence.<path>); host-invariant
//     (incarnation soul values, not per-host data).
//   - Vars         — task-level `vars:` (destiny/tasks.md §9): local task
//     variables already computed by render (CEL expressions over the rest of the
//     context, resolved BEFORE params/where). In CEL available as `vars.<key>`
//     (expression keys) and `${ vars.<key> }` (strings). Scope — one task (and its
//     loop iterations); passed only into the per-task context. nil/empty ⇒
//     accessing `vars.<key>` gives the normal no-such-key.
//
// Loop — `loop:` iteration variables (destiny/tasks.md §7): the name from `as:`
// (default `item`) → the current element, optionally the name from `index_as:` →
// index/key. Names are arbitrary (author-chosen), so with a non-empty Loop
// [Engine.EvalExpression]/[Engine.EvalInterpolation] compile the expression against a
// child env with those names (see [Engine.loopEnv]); they resolve as the bare form
// `<as>.*` in expression keys / `${ <as>.* }` in strings ([ADR-010]). nil/empty Loop
// → ordinary evaluation without loop variables.
//
// [ADR-010]: docs/adr/0010-templating.md
// [soulprint.md]: docs/soul/soulprint.md
type Vars struct {
	Input          map[string]any
	Register       map[string]any
	Incarnation    map[string]any
	SoulprintSelf  map[string]any
	SoulprintHosts []map[string]any
	Essence        map[string]any
	Vars           map[string]any
	Loop           map[string]any

	// Compute — scenario-level computed variables (`compute:`, ADR-009 amendment
	// 2026-06-23): resolved by the Keeper ONCE per run in a run-level context (no
	// soulprint), available in CEL as `compute.<name>`. Scope — apply.input AND
	// state_changes (host-invariant by construction). nil/empty ⇒ `compute.<name>`
	// gives the normal no-such-key. NOT passed into the destiny pass (isolation:
	// destiny sees the result only via apply.input).
	Compute map[string]any

	// State — the root of incarnation.state in migration mode ([NewMigration],
	// [ADR-019]): in CEL available as `state.<path>` (mutated over the course of
	// migration operations). Used ONLY by an Engine built via [NewMigration]; in an
	// ordinary (scenario/destiny) Engine this field is ignored (the activation does
	// not read it). nil ⇒ empty map (accessing `state.<key>` gives the normal
	// no-such-key).
	State map[string]any

	// Ctx — request-scoped context for the CEL vault() function (ReadKV
	// cancel/timeout). Used only when the Engine is built with a KVReader (New +
	// WithVault) and the expression calls vault(); otherwise ignored. nil ⇒
	// context.Background() (vault() without cancellation — acceptable for offline
	// soul-lint/Trial modes).
	Ctx context.Context

	// AllowHosts permits soulprint.hosts/soulprint.where(...) in the expression. true
	// — the scenario pass (host accessor is visible); false (zero-value) — the
	// destiny pass and other contexts without run hosts: accessing soulprint.hosts →
	// isolation error ([orchestration.md §4.1]). Part of the compile-cache key (the
	// compile outcome depends on the flag).
	AllowHosts bool
}

// activation builds the map for cel.NewActivation. soulprint is wrapped in
// {"self": …, "hosts": […]} so the canonical form soulprint.self.<path> and the
// scenario accessor soulprint.hosts resolve, while a bare soulprint.<path> yields a
// missing key (caught separately by the validator — [soulprint.md]). vars —
// task-level `vars:` ([Vars.Vars]): nil ⇒ empty map (accessing `vars.<key>` gives
// the normal no-such-key). Values are already computed by render BEFORE the
// activation is built (vars: resolve before params/where).
//
// soulprint.hosts — list(map(string,dyn)); nil SoulprintHosts ⇒ empty list (access
// in the destiny pass is cut off at compile, [Vars.AllowHosts]).
//
// Loop variables are placed at the top level of the activation under their own names
// (the bare form `<as>.*`). Iteration names do not conflict with the fixed context:
// the config validator forbids `as:`/`index_as:` matching reserved names
// ([scenario_task.go]).
func (v Vars) activation(migration bool) map[string]any {
	var act map[string]any
	if migration {
		// migration mode ([NewMigration]): only `state` is declared. Other context
		// names are NOT placed in the activation — they aren't declared in the env
		// ([migrationVars]) either, so access to them is cut off at compile.
		act = map[string]any{"state": orEmpty(v.State)}
	} else {
		act = map[string]any{
			"input":       orEmpty(v.Input),
			"register":    orEmpty(v.Register),
			"incarnation": orEmpty(v.Incarnation),
			"soulprint":   map[string]any{"self": orEmpty(v.SoulprintSelf), "hosts": orEmptyHosts(v.SoulprintHosts)},
			"essence":     orEmpty(v.Essence),
			"vars":        orEmpty(v.Vars),
			"compute":     orEmpty(v.Compute),
		}
	}
	for name, val := range v.Loop {
		act[name] = val
	}
	return act
}

// loopNames returns the sorted list of loop-variable names (the key of the child env
// and its cache). Empty Loop → nil.
func (v Vars) loopNames() []string {
	if len(v.Loop) == 0 {
		return nil
	}
	names := make([]string, 0, len(v.Loop))
	for name := range v.Loop {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// orEmptyHosts converts []map[string]any to []any for the cel adapter (list elements
// are dyn). nil ⇒ empty list (soulprint.hosts with no hosts).
func orEmptyHosts(hosts []map[string]any) []any {
	out := make([]any, len(hosts))
	for i, h := range hosts {
		out[i] = h
	}
	return out
}
