// Declaring a stand-setup failure, so a red gate says which layer died.
//
// Deliberately NOT behind the `e2e_live` tag, for the same reason as
// waitstrategy.go: the marker below is a contract between this harness and
// everything that reads the gate's log, and a contract that only compiles with
// docker present is one that drifts unobserved.
package harness

import "testing"

// standSetupMarker — what the harness prints when the stand, rather than the
// code under test, is what failed.
//
// NIM-406: every failure inside NewStack arrives as `--- FAIL: TestX`, formatted
// exactly like an assertion that caught a regression. The two have opposite
// answers — one is rerun-the-stand, the other is fix-the-code — and four gate
// failures were read as the second while being the first. Nothing in the output
// distinguished them, because nothing in the harness ever said which had
// happened.
//
// The wording is not free-form. 38 of the 39 L1 integration suites already
// declare setup failures in the shape `<suite> integration: … setup failed: …`,
// and scripts/classify-l1-failure.py keys on exactly that shape. Matching it
// means an e2e-live log fed to that tool is labelled correctly too. That is a
// bonus rather than a dependency: the e2e-live classifier needs no signature
// lists at all, because here the harness states the fact instead of the reader
// inferring it from library text.
const standSetupMarker = "e2e-live integration: stand setup failed"

// declareStandSetupFailure is deferred by the harness entry points that run
// before the test body — NewStack and BuildCommunityRedisPlugin. Deferring it
// once per entry point rather than marking each t.Fatalf is what makes the
// property hold for call sites that do not exist yet: t.Fatalf unwinds through
// runtime.Goexit, which runs defers, so a t.Fatalf added anywhere inside the
// dynamic extent of one of those functions is declared without anyone
// remembering to do it.
//
// `brought` is set true only on the success path, so the declaration fires
// exactly when the entry point was left early. A t.Skipf leaves early too and
// must NOT be declared — a skip asserted nothing but also failed nothing —
// which is why the test is t.Failed() and not "did we reach the end".
//
// If a future entry point forgets this defer, its failures are classified as
// TEST-FAILURE: a person spends time looking for a defect that is not there.
// That is the wrong answer in the safe direction, and it is the direction to be
// wrong in — the opposite mistake, an infra label on a real regression, is the
// one that makes the regression disappear.
func declareStandSetupFailure(t *testing.T, brought *bool) {
	t.Helper()
	if *brought || !t.Failed() {
		return
	}
	t.Logf("%s — no assertion in this test ever ran, so nothing above is a "+
		"finding about the code. Read it as a fact about the machine, and rerun "+
		"this test alone before concluding anything.", standSetupMarker)
}
