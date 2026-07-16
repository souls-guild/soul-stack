package trial

import (
	"sort"

	"github.com/google/cel-go/common/types/ref"
)

// coverageSink implements cel.CoverageSink: tracks trial coverage by
// CEL branches ([ADR-023]). Pilot granularity — "expression truthy/falsy":
// for each unique expression, we track whether true- and
// false-results were seen. Sub-branches within one CEL out of scope ([ADR-023]).
//
// Non-bool results (interpolation of `${ … }` blocks, arithmetic) are tracked
// as "expression executed", but without branch split — they have no meaningful
// truthy/falsy branch in the predicate sense.
type coverageSink struct {
	exprs map[string]*branchState
}

type branchState struct {
	boolean  bool // expression produced bool-result at least once
	sawTrue  bool
	sawFalse bool
}

func newCoverageSink() *coverageSink {
	return &coverageSink{exprs: make(map[string]*branchState)}
}

// Record implements cel.CoverageSink. Called after each successful
// eval; expr is normalized by the engine.
func (s *coverageSink) Record(expr string, out ref.Val) {
	st := s.exprs[expr]
	if st == nil {
		st = &branchState{}
		s.exprs[expr] = st
	}
	if b, ok := out.Value().(bool); ok {
		st.boolean = true
		if b {
			st.sawTrue = true
		} else {
			st.sawFalse = true
		}
	}
}

// CoverageReport aggregates trial coverage upon completion of case(s) run.
type CoverageReport struct {
	// Branches — all bool-expressions (predicates where:/when:/…). Covered
	// expression = both branches (true and false) were seen.
	Branches []BranchCoverage
	// NonBranch — non-bool expressions (interpolations, arithmetic): executed,
	// but without branch split. For text summary "executed N expressions".
	NonBranch []string
}

// BranchCoverage tracks coverage of a single bool-expression.
type BranchCoverage struct {
	Expr     string
	SawTrue  bool
	SawFalse bool
}

// Covered returns true if both branches of the expression were seen.
func (b BranchCoverage) Covered() bool { return b.SawTrue && b.SawFalse }

// Report builds a deterministic (sorted) report.
func (s *coverageSink) Report() CoverageReport {
	var rep CoverageReport
	for expr, st := range s.exprs {
		if st.boolean {
			rep.Branches = append(rep.Branches, BranchCoverage{
				Expr: expr, SawTrue: st.sawTrue, SawFalse: st.sawFalse,
			})
			continue
		}
		rep.NonBranch = append(rep.NonBranch, expr)
	}
	sort.Slice(rep.Branches, func(i, j int) bool { return rep.Branches[i].Expr < rep.Branches[j].Expr })
	sort.Strings(rep.NonBranch)
	return rep
}

// CoveredBranches returns the count of expressions with both branches covered and
// the total count of bool-expressions (for summary "when-branches X/Y").
func (r CoverageReport) CoveredBranches() (covered, total int) {
	total = len(r.Branches)
	for _, b := range r.Branches {
		if b.Covered() {
			covered++
		}
	}
	return covered, total
}
