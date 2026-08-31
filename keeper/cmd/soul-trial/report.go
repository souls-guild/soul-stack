package main

import (
	"fmt"
	"io"

	"github.com/souls-guild/soul-stack/keeper/internal/trial"
)

// printResults prints a text table of results + trial coverage. Returns true
// if all cases pass (an L2-skip run doesn't fail it).
//
// For an L0 case: PASS/FAIL, a `when-branches X/Y, expressions N` summary,
// and on FAIL a list of mismatches; if there are uncovered branches, their
// list. An L1 case (migration test): PASS/FAIL without a coverage summary
// (the render pipeline didn't run for it). An L2 case prints as `SKIP (L2)`
// (stand, ADR-023 post-MVP).
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
		// The identity the own-namespace Vault fence ran against ([ADR-0083] §7). Printed
		// on every case, and marked `(from directory)` when nobody stated it: since NIM-726
		// the manifest names no service, so the harness derives one, and a derived name that
		// is not the REGISTERED name is non-empty and matches nothing — the fence runs and
		// finds nothing, which looks exactly like a clean scenario. This line is the only
		// place the operator can see the word it actually fenced on.
		if r.ServiceStated {
			fmt.Fprintf(w, "    fenced as service %q\n", r.Service)
		} else {
			fmt.Fprintf(w, "    fenced as service %q (from directory; set fixtures.service if that is not the registered name)\n", r.Service)
		}
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

// printUncovered prints bool expressions for which only one branch is covered.
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
