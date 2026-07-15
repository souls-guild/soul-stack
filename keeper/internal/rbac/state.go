package rbac

import (
	"fmt"
	"sync"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// maxStateExprLen is the upper bound on the length of a selector's state CEL
// predicate (ADR-047 S2c). Parallels [maxSoulprintExprLen]/[maxRegexLen]: a
// length cap is cheap insurance against bloated expressions in a snapshot
// (compile cost/memory on load). CEL-go isn't subject to catastrophic
// backtracking; the limit is about size, not ReDoS.
const maxStateExprLen = 512

// stateResolver is the shared validator/eval for RBAC-selector state
// predicates (ADR-047 S2c). Does NOT duplicate the CEL engine: delegates to
// keeper/internal/statepredicate (migration-sandbox root `state`; vault/now/
// register/soulprint/input/essence are forbidden — a state predicate is a
// pure function of incarnation.state). One resolver per process (thread-safe,
// shared shared/cel compile cache) — same pattern as soulprintEngine.
//
// Built lazily, once: the statepredicate.New constructor doesn't depend on
// runtime state (it can only fail on a cel-go build incompatibility), but
// building it in init() would mean paying the cost on every package import;
// lazy construction under sync.Once is cheaper.
var (
	stateResolverOnce sync.Once
	stateResolverInst statepredicate.Resolver
	stateResolverErr  error
)

func stateResolver() (statepredicate.Resolver, error) {
	stateResolverOnce.Do(func() {
		stateResolverInst, stateResolverErr = statepredicate.New()
	})
	return stateResolverInst, stateResolverErr
}

// validateStateExpr compiles state CEL on snapshot load (ADR-047 S2c) via
// statepredicate.Compile: syntax errors and sandbox violations (forbidden
// root/function) both fail the load. A runtime no-such-key on a real
// incarnation is NOT an error (Compile already accounts for this: it
// validates against an empty state). Symmetric with [validateSoulprintExpr],
// but backed by the statepredicate engine.
func validateStateExpr(expr string) error {
	r, err := stateResolver()
	if err != nil {
		return fmt.Errorf("state CEL engine: %w", err)
	}
	return r.Compile(expr)
}

// EvalStateExpr evaluates a state predicate against incarnation.state. Ready
// for S3b (state-based incarnation visibility/resolution): the list/target
// resolver will feed it the incarnation's real state. A thin wrapper over
// statepredicate.Matches — the single source of semantics (no-such-key is a
// fail-closed no-match, non-bool is an author error).
//
// Returns:
//   - (true, nil)  — predicate is true (incarnation is in scope);
//   - (false, nil) — predicate is false OR the needed state fact is absent
//     (no-such-key → fail-closed "not in scope", not an error);
//   - (false, err) — compile error (malformed predicate; normally filtered
//     out at load) or a non-bool predicate result.
func EvalStateExpr(expr string, state map[string]any) (bool, error) {
	r, err := stateResolver()
	if err != nil {
		return false, fmt.Errorf("state CEL engine: %w", err)
	}
	return r.Matches(expr, state)
}
