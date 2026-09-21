//go:build integration

package integrationenv

import (
	"os"
	"testing"
)

// SkipRaceEnv is the opt-OUT for [TestIntegrationSuiteRunsUnderRace]. Set it for
// a deliberately fast local loop; leaving it unset is how you say "this run is
// the real thing".
const SkipRaceEnv = "SOUL_STACK_INTEGRATION_SKIP_RACE"

// TestIntegrationSuiteRunsUnderRace fails a whole-tree integration sweep that
// lost `-race`.
//
// `make test-integration` — the target CI runs — passes `-race`. Sessions
// running L1 by hand typed `go test -tags=integration ./internal/...` instead,
// which does not. Both print the same word at the end, so "L1 is green" has
// meant two different things depending on who said it, and the weaker meaning
// is silent about an entire class of defect: every data race in the code these
// suites exercise.
//
// This is the NIM-238 shape one level up. There, an absent variable made a
// suite skip and report success; here, an absent flag makes a suite run with
// less detection and report success. In both cases the fix is not to document
// the flag harder — it is to make the weaker run say so.
//
// SCOPE, stated rather than implied: this guard lives in one package, so it
// fires on a sweep that includes it (`./...`, which is what the Makefile target
// and CI run) and NOT on a single-package invocation elsewhere. That is why the
// Makefile grew `PKG=` — so a one-package run goes through the same target and
// keeps the flag, instead of being a second, quieter way to run L1. Putting a
// copy of this check in all ~31 suites would catch the remaining case, at the
// cost of re-forking a decision into 31 places, which is exactly what NIM-238
// spent its time undoing.
func TestIntegrationSuiteRunsUnderRace(t *testing.T) {
	if RaceEnabled {
		return
	}
	if v := os.Getenv(SkipRaceEnv); v != "" {
		t.Skipf("race detector off, %s=%q — declared, not accidental", SkipRaceEnv, v)
	}
	t.Fatalf("this integration sweep is running WITHOUT the race detector, which CI does not do.\n"+
		"Run it the way CI does:\n"+
		"    make test-integration               # whole tree\n"+
		"    make test-integration PKG=./internal/scenario/\n"+
		"or say out loud that this run is the weaker one:\n"+
		"    %s=1 go test -tags=integration ./...\n"+
		"A green result from a sweep without -race is silent about every data race in the code it exercised.",
		SkipRaceEnv)
}
