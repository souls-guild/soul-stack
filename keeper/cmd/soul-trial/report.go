package main

import (
	"fmt"
	"io"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
)

// printResults печатает текст-таблицу результатов + trial coverage. Возвращает
// true, если все кейсы pass (L2-skip прогон не валит).
//
// На L0-кейс: PASS/FAIL, сводка `when-branches X/Y, expressions N`, при FAIL —
// список расхождений, при наличии непокрытых веток — их список. L1-кейс (тест
// миграции): PASS/FAIL без coverage-сводки (render-пайплайн на нём не гонялся).
// L2-кейс печатается как `SKIP (L2)` (стенд, ADR-023 post-MVP).
func printResults(w io.Writer, results []trial.Result) bool {
	allPass := true
	var passL0, passL1, skippedL2 int

	for _, r := range results {
		if r.Level == trial.LevelL2 {
			skippedL2++
			fmt.Fprintf(w, "SKIP  %s  (L2)\n", r.Case)
			continue
		}

		status := "PASS"
		if !r.Pass {
			status = "FAIL"
			allPass = false
		} else if r.Level == trial.LevelL1 {
			passL1++
		} else {
			passL0++
		}

		if r.Level == trial.LevelL1 {
			fmt.Fprintf(w, "%s  %s  (L1)\n", status, r.Case)
			for _, f := range r.Failures {
				fmt.Fprintf(w, "    - %s\n", f)
			}
			continue
		}

		coveredBranches, totalBranches := r.Coverage.CoveredBranches()
		fmt.Fprintf(w, "%s  %s\n", status, r.Case)
		fmt.Fprintf(w, "    when-branches %d/%d, expressions %d\n",
			coveredBranches, totalBranches, len(r.Coverage.Branches)+len(r.Coverage.NonBranch))

		for _, f := range r.Failures {
			fmt.Fprintf(w, "    - %s\n", f)
		}

		printUncovered(w, r.Coverage)
	}

	fmt.Fprintf(w, "\n%d L0 passed, %d L1 passed, %d L2 skipped\n", passL0, passL1, skippedL2)
	return allPass
}

// printUncovered печатает bool-выражения, у которых покрыта только одна ветка.
func printUncovered(w io.Writer, cov trial.CoverageReport) {
	var uncovered []trial.BranchCoverage
	for _, b := range cov.Branches {
		if !b.Covered() {
			uncovered = append(uncovered, b)
		}
	}
	if len(uncovered) == 0 {
		return
	}
	fmt.Fprintln(w, "    uncovered branches:")
	for _, b := range uncovered {
		fmt.Fprintf(w, "      %q (true=%v, false=%v)\n", b.Expr, b.SawTrue, b.SawFalse)
	}
}
