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

// shouldDeclare — the whole decision, as a function of three booleans, so
// setupdecl_test.go can exercise it exhaustively without needing a failed
// *testing.T or a way to capture t.Logf.
//
//	infraUp      the infrastructure region was left behind normally
//	failedBefore the test was ALREADY red when the entry point was called
//	failedNow    the test is red now
//
// `failedBefore` is what makes this three booleans instead of two, and it is not
// hypothetical. t.Failed() reports the test as a whole: (*common).Fail walks to
// the parent, so a failed SUBTEST marks its parent failed immediately. Without
// the before-snapshot the condition reads "this test is red by now" rather than
// "this region turned it red", and any entry point called after a t.Errorf — a
// second NewStack, or one that hits the binary-missing t.Skipf inside — would
// stamp STAND-SETUP over a finding that was already recorded. That is the one
// direction this mechanism must never be wrong in (NIM-507 follow-up).
func shouldDeclare(infraUp, failedBefore, failedNow bool) bool {
	return !infraUp && failedNow && !failedBefore
}

// declareStandSetupFailure is deferred by the harness entry points, over the
// region of them that is INFRASTRUCTURE — docker, tempdirs, TLS material, the
// third-party containers — and not over the region that runs this repo's own
// binaries. Deferring it once per region rather than marking each t.Fatalf is
// what makes the property hold for call sites that do not exist yet: t.Fatalf
// unwinds through runtime.Goexit, which runs defers, so a t.Fatalf added
// anywhere inside the dynamic extent is declared without anyone remembering to
// do it.
//
// Where the region ENDS is the load-bearing part, and getting it wrong is not a
// small error. The first cut of NIM-406 set `infraUp` on the last line of
// NewStack, which put `keeper init`, `keeper run`, POST /v1/services and the
// whole Soul onboarding path (`soul init` = CSR Bootstrap, `soul run`, waiting
// for souls.status='connected') INSIDE the declared region. That onboarding path
// runs nowhere else — tests/e2e has no real soul binary at all — so a regression
// in it would have printed STAND-SETUP on all nine gate tests, under the words
// "no assertion in this test ever ran", and a gate red in nothing but
// STAND-SETUP reads exactly like a bad day for docker. An infra label on a real
// regression is how a regression disappears. Set the flag as soon as the
// third-party plumbing is up, not when the function is done.
//
// A t.Skipf leaves the region early too and must NOT be declared — a skip
// asserted nothing but also failed nothing — which is why the test is
// t.Failed() and not "did we reach the end".
//
// If a future entry point forgets this defer, its failures are classified as
// TEST-FAILURE: a person spends time looking for a defect that is not there.
// That is the wrong answer in the safe direction, and it is the direction to be
// wrong in.
//
// Call it as `defer declareStandSetupFailure(t, t.Failed(), &infraUp)`. The
// t.Failed() argument is evaluated when the defer is REGISTERED, which is
// exactly the entry snapshot shouldDeclare needs — do not "fix" it into a
// closure that re-reads it at exit, which would silently restore the
// stamp-over-a-finding case.
func declareStandSetupFailure(t *testing.T, failedBefore bool, infraUp *bool) {
	t.Helper()
	if !shouldDeclare(*infraUp, failedBefore, t.Failed()) {
		return
	}
	t.Logf("%s — the stand's infrastructure never came up, so nothing above is a "+
		"finding about the code. Read it as a fact about the machine, and rerun "+
		"this test alone before concluding anything.", standSetupMarker)
}
