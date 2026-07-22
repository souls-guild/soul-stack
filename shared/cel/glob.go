package cel

import (
	"path/filepath"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// CEL function glob() ([templating.md "Custom CEL functions"], [ADR-040]).
// Shell-glob matching of a string against a pattern — glob matcher `-G`:
//
//	'sid.glob("prod-*")'                          → bool
//	'soulprint.self.os.family.glob("debian*")'   → bool
//
// Pure over two strings (no I/O, network, or eval-time state), symmetric with the
// stdlib size()/contains(). Implemented via [filepath.Match] (`*`/`?`/`[abc]`/
// `[a-z]`, escape via `\\`). A broken pattern → false without error: target.where
// in Tide/ErrandRun must not fail on a per-host predicate — an unevaluable pattern
// on a host is treated as "no match", syntax validation is done by [soul-lint] at
// scenario compile time.
//
// Member overload `s.glob(p)` (string receiver + string arg) symmetric with the
// stdlib `s.contains(p)`/`s.matches(re)`. The global form `glob(s, p)` is not
// registered — a single way to write it in Destiny/scenario.
//
// Registered only in the main scenario/destiny mode (see [buildEngine]):
// migration-CEL ([ADR-019]) is a hermetic sandbox (only `state` + pure
// arithmetic/stdlib operations), extending it with custom functions needs a
// separate ADR. Flow-control mode ([NewFlowControl], [ADR-012(d)]) does get glob():
// when:/changed_when:/failed_when: predicates on the Soul are symmetric with
// scenario predicates, and glob() pulls no external context.

// globFuncName — the function name in the CEL env. The user writes `s.glob(p)`.
const globFuncName = "glob"

// globEnvOptions returns the EnvOptions that register glob(): a single
// member overload `string.glob(string) bool`. Called from [buildEngine] for all
// modes EXCEPT migration (migration-CEL stays hermetic).
func globEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function(globFuncName,
			cel.MemberOverload("string_glob_string",
				[]*cel.Type{cel.StringType, cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(callGlob),
			),
		),
	}
}

// callGlob — the binding for `<string>.glob(<pattern>)`. CEL guaranteed both
// arguments to string at type-check (the overload is pinned to StringType). A
// broken pattern ([filepath.ErrBadPattern]) → false: the per-host predicate
// target.where must not fail on an individual host — the pattern syntax is
// validated by soul-lint before the run.
func callGlob(strVal, patternVal ref.Val) ref.Val {
	s, ok := strVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	pattern, ok := patternVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	matched, err := filepath.Match(pattern, s)
	if err != nil {
		return types.Bool(false)
	}
	return types.Bool(matched)
}
