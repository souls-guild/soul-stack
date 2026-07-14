package cel

import (
	"strconv"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// The merge() CEL function ([templating.md §2.3], [ADR-010 Amendment 2026-06-22]).
// Merges maps left to right by TOP-level key. Two forms:
//
//	${ merge(essence.redis.defaults, input.redis_settings) }   → map  (varargs)
//	${ merge(a, b, c) }                                         → map  (varargs)
//	${ merge(input.users.map(name, {...})) }                   → map  (one list(map))
//
// The second form takes ONE list-of-maps argument and flattens it left to right:
// the same merge, but over the list elements rather than positional arguments. Covers
// the case where a collection comes from a CEL comprehension `.map(...)` (yielding a
// list) but must be passed to the template as a "name→object" map for DETERMINISTIC
// row order (text/template range over a map sorts keys; over a list it preserves the
// possibly non-deterministic Go-map iteration order).
//
// SHALLOW (a nested map is NOT deep-merged — the right argument replaces the whole
// value of a matching top key), last-wins (right overrides left). Pure: no I/O,
// network, secrets, crypto, or eval-time state — symmetric to the stdlib size()/glob()
// functions. Translation slot "simple typed operator input → detailed config": base
// CEL has no map+map operator, and the engine boundary ([ADR-010]) doesn't allow
// sprig-merge in `.yml` — merge() covers merging an author preset with a
// passthrough input map in the render phase.
//
// Result type is map(dyn,dyn) (like the other nodes until the type-model is closed
// [templating.md §2.4]). A non-map argument → eval error (types.NewErr, not a panic):
// CEL provides an overload only for map arguments, but on dyn-splicing of an
// uncomputable type the binding gets a non-mapper ref.Val — we return a clear error.
//
// Security ([ADR-010 Amendment 2026-06-22], security-blocker): merge() keeps
// top-level keys without renaming, so a merged map with a vault value (${ vault(...) }
// under a SENSITIVE destination key — password/secret/token/…) is masked by the output
// layer (shared/audit.MaskSecrets by sensitive key name) identically to a direct
// ${ vault(...) } under the same key.
//
// The vault-ref-marker branch of MaskSecrets ([vaultRefRe]) does NOT fire here:
// vault() resolves to plaintext keeper-side, so the merged map holds the secret value,
// not a `vault:<mount>/…` ref string. Hence a secret under a NON-sensitive key is NOT
// masked — that's an invariant on scenario authors (put a secret under a secret-named
// key), NOT merge()'s responsibility. Proven by
// [TestMerge_SecretMaskedSameAsDirectVault] (sensitive key is masked) +
// [TestMerge_SecretUnderNonSensitiveKeyNotMasked] (non-secret key is not).
//
// Registered in the main scenario/destiny mode (Keeper full env) and in flow-control
// mode ([NewFlowControl], [ADR-012(d)]): a pure function with no external context,
// symmetric to scenario expressions. In migration-CEL ([ADR-019]) it is NOT registered
// (see [buildEngine]): a hermetic sandbox with minimal surface area (only `state` +
// stdlib), extending it needs a separate ADR (like glob()).

// mergeFuncName — the function name in the CEL env. The user writes `merge(a, b, ...)`.
const mergeFuncName = "merge"

// mergeEnvOptions returns the EnvOptions registering merge(): a global variadic
// function via overloads for 1..N map arguments. cel-go has no native variadic
// overload, so we declare fixed arities up to mergeMaxArity (covers real scenarios:
// merging preset layers; beyond that — a clear no-such-overload compile error,
// extensible without a breaking change).
func mergeEnvOptions() []cel.EnvOption {
	mapType := cel.MapType(cel.DynType, cel.DynType)
	listOfMapType := cel.ListType(mapType)
	overloads := make([]cel.FunctionOpt, 0, mergeMaxArity+1)
	for arity := 1; arity <= mergeMaxArity; arity++ {
		args := make([]*cel.Type, arity)
		for i := range args {
			args[i] = mapType
		}
		overloads = append(overloads, cel.Overload(
			overloadName(arity), args, mapType,
			cel.FunctionBinding(callMerge),
		))
	}
	// merge(list(map)) -> map: ONE list-of-maps argument, flattened left to right.
	// A separate overload by argument type (list(map) ≠ map), so it doesn't conflict
	// with the arity-1 varargs form merge(map): cel-go dispatches by type.
	overloads = append(overloads, cel.Overload(
		mergeFuncName+"_listmap_map", []*cel.Type{listOfMapType}, mapType,
		cel.UnaryBinding(callMergeList),
	))
	return []cel.EnvOption{cel.Function(mergeFuncName, overloads...)}
}

// mergeMaxArity — the maximum number of map arguments merge() has a declared overload
// for. Merging preset layers fits in a few arguments in practice; beyond that —
// extensible without a breaking change (add overloads).
const mergeMaxArity = 8

// overloadName — a deterministic overload name for an arity of arguments (cel-go
// requires a unique name per overload).
func overloadName(arity int) string {
	return mergeFuncName + "_" + strconv.Itoa(arity) + "map_map"
}

// callMerge — binding for merge(m, m, ...). SHALLOW last-wins merge: sequential
// left-to-right pass over arguments, top-level keys overwritten (right beats left).
// A non-map argument (dyn-splicing yielded an uncomputable type) → types.NewErr (a
// normal eval error). The result is assembled into map[ref.Val]ref.Val and adapted to
// a CEL map via types.NewRefValMap.
func callMerge(args ...ref.Val) ref.Val {
	out := make(map[ref.Val]ref.Val)
	for _, a := range args {
		if err := mergeInto(out, a); err != nil {
			return err
		}
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, out)
}

// callMergeList — binding for the merge(list(map)) form. The single list-of-maps
// argument is flattened left to right by the same SHALLOW last-wins rule. Empty list →
// empty map. A non-map list element (dyn list) → a clear eval error.
func callMergeList(arg ref.Val) ref.Val {
	l, ok := arg.(traits.Lister)
	if !ok {
		return types.NewErr("merge(): аргумент должен быть list(map), получено %s", arg.Type().TypeName())
	}
	out := make(map[ref.Val]ref.Val)
	it := l.Iterator()
	for it.HasNext() == types.True {
		if err := mergeInto(out, it.Next()); err != nil {
			return err
		}
	}
	return types.NewRefValMap(types.DefaultTypeAdapter, out)
}

// mergeInto merges one map argument into the accumulator out (top-level keys,
// last-wins). A non-map argument → types.NewErr (returned by the caller as is).
func mergeInto(out map[ref.Val]ref.Val, a ref.Val) ref.Val {
	m, ok := a.(traits.Mapper)
	if !ok {
		return types.NewErr("merge(): элемент должен быть map, получено %s", a.Type().TypeName())
	}
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		out[k] = m.Get(k)
	}
	return nil
}
