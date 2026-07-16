// Package statepredicate is a unified incarnation resolver by CEL predicate over
// incarnation.state. Foundation for three consumers (DO NOT duplicate
// mechanism):
//   - incarnation filter -> Run late-binding (target = state predicate, resolved
//     at run start);
//   - RBAC Purview S2c state selector (Purview.StateExprs);
//   - Cadence late-binding by state.
//
// Compile (validation + program cache) + Matches (single-incarnation check
// against state map) are the foundation. ResolveIncarnations (list + CEL filter
// over narrowed set) is added on top: resolver itself does NOT know SQL; list
// access is encapsulated in [IncarnationStateLister], pushdown ([BaseFilter] by
// service/coven) lives in consumer's lister implementation, and resolver only
// runs [Matches] over the already narrowed set. DB coupling is not pulled into
// the package; lister is trivial to mock in tests.
//
// CEL engine is NOT duplicated: shared/cel is reused in migration mode
// ([cel.NewMigration]), the project's only sandbox with root `state` (ADR-019).
// Semantically, state predicate is a pure function of state, exactly matching
// migration-CEL sandbox: only `state.<path>` is declared; other roots
// (register/soulprint/essence/input/incarnation/vars) are undeclared (compile
// error undeclared reference), vault()/now() are cut by guards. Same approach as
// rbac.soulprint (S2b) with [cel.NewFlowControl].
package statepredicate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Resolver compiles and evaluates state predicates. It is thread-safe
// (compile-cache inside shared/cel.Engine under RWMutex).
type Resolver interface {
	// Compile validates predicate (syntax + sandbox) and warms program cache.
	// Called on load (filter/RBAC selector/Cadence target validate predicate
	// before run). Empty/blank predicate is rejected.
	//
	// Errors are compile phase: broken CEL, access to forbidden root/function
	// (vault/now/register/soulprint/input/incarnation/essence). Actual absence
	// of state fact at runtime is NOT a Compile error (a concrete incarnation may
	// have the fact).
	Compile(predicate string) error

	// Matches checks one incarnation: whether predicate is true for its state.
	//
	// Return:
	//   - (true, nil)  - predicate is true (incarnation in selection);
	//   - (false, nil) - false OR needed state fact is absent (no-such-key ->
	//     fail-closed: "did not match", not an error);
	//   - (false, err) - compile error (broken/sandbox predicate; normally cut
	//     by Compile) or non-bool predicate result.
	//
	// Semantics "runtime-no-match = (false, nil)" is symmetric to
	// rbac.EvalSoulprintExpr and oracle.WhereEvaluator: incomplete state snapshot
	// must not break resolver.
	Matches(predicate string, state map[string]any) (bool, error)

	// ResolveIncarnations returns names of incarnations whose state satisfies
	// predicate. Perf strategy is two-stage pushdown:
	//   1. SQL pushdown: base ([BaseFilter] service/coven) narrows set on lister
	//      side (SQL WHERE) BEFORE CEL eval (100k fleet -> service/coven subset);
	//   2. page-by-page: lister streams narrowed set by pages (see
	//      [IncarnationStateLister.ListStatePages]), resolver runs [Matches]
	//      per incarnation on EACH page and releases it immediately. Whole set is
	//      not materialized in memory at once (important for large service whose
	//      subset is still large).
	// Resolver itself does not execute SQL; lister provides list access and
	// pagination.
	//
	// predicate is compiled once (Compile semantics): empty/broken/sandbox/
	// non-bool is an error BEFORE walking the set. Per-incarnation no-such-key is
	// fail-closed (incarnation simply not in selection, see [Matches]). Lister
	// error is propagated.
	ResolveIncarnations(ctx context.Context, predicate string, base BaseFilter, lister IncarnationStateLister) ([]string, error)
}

// BaseFilter is SQL-pushdown pre-filter (service/coven) for narrowing the set
// of incarnations BEFORE CEL eval. Empty fields mean "do not filter".
// Intentionally does NOT import incarnation.ListFilter: resolver does not know
// about repository; consumer adapter maps these fields into incarnation.ListFilter
// itself (see [IncarnationStateLister]).
//
// Coven (single) and Covens (multi) are two paths of one dimension,
// "coven narrowing":
//   - Coven - exact any-of by ONE label (`$n = ANY(covens)`); previous
//     Run/Cadence late-binding path.
//   - Covens - multi-coven any-of IN coven union {name} MODE (ADR-008 amendment
//     a): label matches both `covens[] && ARRAY[Covens]` and
//     `name = ANY(Covens)` (incarnation name = root Coven label). Added
//     additively for S3b-3 RBAC-scope resolution: scope-coven `redis-prod` must
//     match incarnation both with covens containing redis-prod and with
//     name=redis-prod. Empty means do not filter.
//
// Both fields are additive; when both are set, adapter AND-combines them (in
// practice consumer uses one). Old single path (Coven) is untouched.
type BaseFilter struct {
	Service string
	Coven   string
	Covens  []string
}

// Stated is pair "incarnation name + its state" from lister. Minimal projection
// required by resolver: other incarnation row fields are not needed for CEL
// filter by state.
type Stated struct {
	Name  string
	State map[string]any
}

// IncarnationStateLister is narrow list access decoupling resolver from DB.
// Adapter implementation reuses incarnation.SelectAll for SQL pushdown by base
// (service/coven) and returns already narrowed set by PAGES through callback;
// whole set is not materialized in memory at once (architect page-by-page
// strategy: subset of large service may itself be large).
//
// ListStatePages calls yield for each page until set is exhausted. Contract:
//   - pages are non-empty and in stable order (pagination does not jump);
//   - error from yield is propagated outward and interrupts walk (resolver uses
//     this to return non-bool / other eval error);
//   - page read error from DB is propagated outward.
//
// Implementation ([incarnation.StateLister]) lives at consumer/in incarnation
// package, not here: otherwise statepredicate would pull direct dependency on
// incarnation + pgx, breaking testability (here lister is mocked without PG).
type IncarnationStateLister interface {
	ListStatePages(ctx context.Context, base BaseFilter, yield func(page []Stated) error) error
}

// resolver is Resolver implementation over shared/cel sandbox Engine (migration
// mode, root `state`).
type resolver struct {
	engine *cel.Engine
}

// stateEngine is shared sandbox engine for state predicates. Built lazily once
// per process (constructor does not depend on runtime; building in init() means
// paying on every package import). Thread-safe, reused by all Resolvers: single
// compile cache per process.
var (
	stateEngineOnce sync.Once
	stateEngineInst *cel.Engine
	stateEngineErr  error
)

func stateEngine() (*cel.Engine, error) {
	stateEngineOnce.Do(func() {
		stateEngineInst, stateEngineErr = cel.NewMigration()
	})
	return stateEngineInst, stateEngineErr
}

// New creates Resolver. Error is possible only on programming incompatibility
// with cel-go (not user-facing).
func New() (Resolver, error) {
	e, err := stateEngine()
	if err != nil {
		return nil, fmt.Errorf("state-predicate CEL engine: %w", err)
	}
	return &resolver{engine: e}, nil
}

func (r *resolver) Compile(predicate string) error {
	if strings.TrimSpace(predicate) == "" {
		return errors.New("blank state predicate (expected CEL expression over state.*; do not call resolver for select-all)")
	}
	// Validation = eval against EMPTY state. Raise compile errors (syntax,
	// forbidden root, vault/now); runtime no-such-key on empty state is NOT a
	// load error (real incarnation may have the fact). Same approach as
	// rbac.validateSoulprintExpr.
	_, evalErr := r.engine.EvalPredicate(predicate, cel.Vars{State: map[string]any{}})
	if evalErr == nil {
		return nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return evalErr
	}
	// ErrEval on empty state (no-such-key / non-bool on missing key) is a
	// syntactically valid expression; do not fail load.
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		return nil
	}
	// Other cases (theoretically unreachable) fail closed.
	return evalErr
}

func (r *resolver) Matches(predicate string, state map[string]any) (bool, error) {
	if strings.TrimSpace(predicate) == "" {
		return false, errors.New("blank state predicate")
	}
	ok, evalErr := r.engine.EvalPredicate(predicate, cel.Vars{State: state})
	if evalErr == nil {
		return ok, nil
	}
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(evalErr, &ce) || errors.As(evalErr, &ue) {
		return false, evalErr
	}
	// Non-bool result and runtime no-such-key are both ErrEval. Distinguish by
	// typed shared/cel signal (sentinel ErrPredicateNotBool wrapped inside
	// ErrEval), robust to message text changes unlike previous strings.Contains.
	//   - non-bool -> predicate author error (predicate must be boolean), return it;
	//   - other runtime (no-such-key etc.) -> fail-closed (false, nil).
	var ee *cel.ErrEval
	if errors.As(evalErr, &ee) {
		if errors.Is(evalErr, cel.ErrPredicateNotBool) {
			return false, evalErr
		}
		return false, nil
	}
	return false, evalErr
}

func (r *resolver) ResolveIncarnations(ctx context.Context, predicate string, base BaseFilter, lister IncarnationStateLister) ([]string, error) {
	// Validate predicate ONCE before walking the set: empty/broken/sandbox/
	// non-bool are cut here, so we do not pay eval for each incarnation and do
	// not return partial selection for a broken expression. Compile warms shared
	// Engine program cache; subsequent Matches reuse program.
	if err := r.Compile(predicate); err != nil {
		return nil, err
	}

	var out []string
	pageErr := lister.ListStatePages(ctx, base, func(page []Stated) error {
		for i := range page {
			ok, err := r.Matches(predicate, page[i].State)
			if err != nil {
				// Compile (against empty state) does not catch non-bool if full state
				// makes predicate produce non-boolean result (no-such-key on empty
				// state masks it), so not-bool may surface only here. Fail-closed:
				// do not swallow it; interrupt walk (broken author predicate, not
				// incarnation data).
				return fmt.Errorf("state-predicate eval %q: %w", page[i].Name, err)
			}
			if ok {
				out = append(out, page[i].Name)
			}
		}
		return nil
	})
	if pageErr != nil {
		return nil, fmt.Errorf("state-predicate list: %w", pageErr)
	}
	return out, nil
}
