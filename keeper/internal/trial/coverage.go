package trial

import (
	"sort"

	"github.com/google/cel-go/common/types/ref"
)

// coverageSink реализует cel.CoverageSink: учитывает trial coverage по
// CEL-веткам ([ADR-023]). Гранулярность пилота — «выражение truthy/falsy»:
// для каждого уникального выражения отмечается, встречались ли true- и
// false-результаты. Под-ветки внутри одного CEL вне scope ([ADR-023]).
//
// Не-bool результаты (интерполяция `${ … }`-блоков, арифметика) учитываются
// как «выражение прогнано», но без branch-разбивки — у них нет осмысленной
// truthy/falsy-ветки в смысле предиката.
type coverageSink struct {
	exprs map[string]*branchState
}

type branchState struct {
	boolean  bool // выражение хоть раз дало bool-результат
	sawTrue  bool
	sawFalse bool
}

func newCoverageSink() *coverageSink {
	return &coverageSink{exprs: make(map[string]*branchState)}
}

// Record — реализация cel.CoverageSink. Вызывается после каждого успешного
// eval; expr нормализован движком.
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

// CoverageReport — агрегат trial coverage по завершении прогона кейса(ов).
type CoverageReport struct {
	// Branches — все bool-выражения (предикаты where:/when:/…). Покрытое
	// выражение = обе ветки (true и false) встретились.
	Branches []BranchCoverage
	// NonBranch — не-bool выражения (интерполяции, арифметика): прогнаны,
	// но без branch-разбивки. Для текстовой сводки «прогнано N выражений».
	NonBranch []string
}

// BranchCoverage — покрытие одного bool-выражения.
type BranchCoverage struct {
	Expr     string
	SawTrue  bool
	SawFalse bool
}

// Covered — обе ветки выражения встретились.
func (b BranchCoverage) Covered() bool { return b.SawTrue && b.SawFalse }

// Report строит детерминированный (отсортированный) отчёт.
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

// CoveredBranches возвращает число выражений с обеими покрытыми ветками и
// общее число bool-выражений (для сводки «when-branches X/Y»).
func (r CoverageReport) CoveredBranches() (covered, total int) {
	total = len(r.Branches)
	for _, b := range r.Branches {
		if b.Covered() {
			covered++
		}
	}
	return covered, total
}
