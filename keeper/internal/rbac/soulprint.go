package rbac

import (
	"errors"
	"fmt"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// maxSoulprintExprLen is the upper bound on the length of a selector's
// soulprint CEL predicate (ADR-047 S2b). Parallels [maxRegexLen]: the length
// cap is cheap insurance against bloated expressions in the snapshot
// (compile cost/memory on load). CEL-go isn't subject to catastrophic
// backtracking, so this limits size, not ReDoS.
const maxSoulprintExprLen = 512

// soulprintEngine is the shared validator/eval engine for RBAC selector
// soulprint predicates. Sandbox mode [cel.NewFlowControl]: `soulprint.self.*`
// is declared (ADR-018 canonical form), vault()/now()/register/state are
// forbidden (a scope predicate must be a pure function of host facts), and
// soulprint.hosts/soulprint.where are cut off by the allowHosts=false
// isolation. The engine is thread-safe (compile cache under RWMutex) and
// reused by every Check/load — we don't duplicate the project's CEL engine.
//
// Built lazily, once: the [cel.NewFlowControl] constructor doesn't depend on
// runtime state (it only errors on a cel-go build incompatibility), but
// building it in init() would mean paying the cost on every package import;
// lazy construction under sync.Once is cheaper.
var (
	soulprintEngineOnce sync.Once
	soulprintEngineInst *cel.Engine
	soulprintEngineErr  error
)

func soulprintEngine() (*cel.Engine, error) {
	soulprintEngineOnce.Do(func() {
		soulprintEngineInst, soulprintEngineErr = cel.NewFlowControl()
	})
	return soulprintEngineInst, soulprintEngineErr
}

// validateSoulprintExpr compiles a soulprint CEL predicate at snapshot load
// time (ADR-047 S2b). Only the compile phase is fatal: syntax, an unknown
// root (register/state/vault/now), a host accessor (soulprint.hosts →
// isolation error). A runtime no-such-key (the predicate references a fact
// absent from the EMPTY validation facts) is NOT a load error (the fact will
// exist on a real host): eval against an empty SoulprintSelf yields
// [cel.ErrEval], which we swallow. This way a broken CEL predicate fails load
// (like a broken regex / unknown permission), while a valid one doesn't —
// without feeding it fake facts.
func validateSoulprintExpr(expr string) error {
	e, err := soulprintEngine()
	if err != nil {
		return fmt.Errorf("soulprint CEL engine: %w", err)
	}
	_, evalErr := e.EvalPredicate(expr, cel.Vars{SoulprintSelf: map[string]any{}})
	if evalErr == nil {
		return nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return evalErr
	}
	// [cel.ErrEval] on empty facts (no-such-key, non-bool on a missing key) is
	// expected — the expression is syntactically valid.
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		return nil
	}
	// Anything else (theoretically unreachable) — fail-closed: load fails.
	return evalErr
}

// EvalSoulprintExpr evaluates a soulprint predicate against host facts
// (`soulprint.self.*`, ADR-018). Ready for the S3/S4 slices (list
// visibility/target): the list/target resolver feeds in the host's real
// SoulprintFacts.
//
// Returns:
//   - (true, nil)  — the predicate is true (host is in scope);
//   - (false, nil) — false, OR the fact is missing (no-such-key →
//     default-deny: a missing fact means "not in scope", not an error);
//   - (false, err) — compile error (broken predicate; normally caught at
//     load time).
//
// The "runtime no-match = (false, nil)" semantics mirror
// oracle.WhereEvaluator and the flow-control predicates: an untrusted/
// incomplete facts snapshot must not crash the resolver — a missing fact is
// treated as "didn't match".
func EvalSoulprintExpr(expr string, facts map[string]any) (bool, error) {
	e, err := soulprintEngine()
	if err != nil {
		return false, fmt.Errorf("soulprint CEL engine: %w", err)
	}
	ok, evalErr := e.EvalPredicate(expr, cel.Vars{SoulprintSelf: facts})
	if evalErr == nil {
		return ok, nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return false, evalErr
	}
	// Runtime (no-such-key / non-bool) → no-match (default-deny).
	return false, nil
}
