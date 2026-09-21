//go:build e2e

package harness

import "testing"

// Declaring a stand-setup failure, so a red L3a says which layer died.
//
// The mechanism is the one NIM-406 landed for e2e-live
// (tests/e2e-live/harness/setupdecl.go) and the reasoning transfers unchanged,
// so this is deliberately a sibling rather than a variation: same marker shape,
// same three-boolean decision, same rule about where the region ends. What
// differs is only the tier's name in the marker and where the boundary falls.

// standSetupMarker — what the harness prints when the stand, rather than the
// code under test, is what failed.
//
// NIM-469: L3a's whole signature is that every test passes alone and a full
// suite drops two or three of them, a different set each time. Read as a list of
// `--- FAIL:` lines those are indistinguishable — an assertion that caught a
// regression and a Vault container that never answered print the same shape —
// so the run gets called flaky and rerun, and a suite nobody believes certifies
// nothing. R5 finishes on trust in the verification, so an illegible red L3a is
// a release blocker in its own right.
//
// The wording follows the `<suite> integration: … setup failed` shape that 38 of
// the 39 L1 suites already use and that scripts/classify-l1-failure.py keys on,
// which means an L3a log fed to that tool is labelled correctly too.
//
// That shape is a constraint on the wording, not decoration. L3a's own
// classifier reads this constant instead of copying it, so it follows any
// rewording — which is exactly why nothing here noticed when a mutation run
// reworded it to "stand could not be set up": both sides moved together and the
// self-test stayed green, while L1's pattern silently stopped matching. So the
// self-test now reads that pattern out of classify-l1-failure.py and checks this
// string against it, and rewording past the shape fails `make check-e2e-set`.
const standSetupMarker = "e2e integration: stand setup failed"

// shouldDeclare — the whole decision, as a function of three booleans, so
// setupdecl_test.go can exercise it exhaustively without needing a failed
// *testing.T or a way to capture t.Logf.
//
//	infraUp      the infrastructure region was left behind normally
//	failedBefore the test was ALREADY red when the entry point was called
//	failedNow    the test is red now
//
// `failedBefore` is what makes this three booleans instead of two. t.Failed()
// reports the test as a whole — (*common).Fail walks to the parent, so a failed
// subtest marks its parent failed immediately — and L3a calls NewStack from
// tests that have already asserted things. Without the entry snapshot the
// condition would read "this test is red by now" rather than "this region turned
// it red", and any second stand would stamp STAND-SETUP over a finding that was
// already recorded. That is the one direction this mechanism must never be
// wrong in.
func shouldDeclare(infraUp, failedBefore, failedNow bool) bool {
	return !infraUp && failedNow && !failedBefore
}

// declareStandSetupFailure is deferred by the harness entry points over the
// region of them that is INFRASTRUCTURE — docker, the tempdir, the TLS
// material, the three third-party containers and the clients that talk to them
// — and NOT over the region that runs this repo's own binaries. Deferring once
// per region rather than marking each t.Fatalf is what makes the property hold
// for call sites that do not exist yet: t.Fatalf unwinds through
// runtime.Goexit, which runs defers, so a t.Fatalf added anywhere inside the
// dynamic extent is declared without anyone remembering to do it.
//
// Where the region ENDS is the load-bearing part. In NewStack it ends at
// `keeper init`; everything from there on — `keeper init`, `keeper run`,
// /readyz, assertOwnKeeper, RegisterSoulPreAuth — is this repo's code, and a
// regression in any of it must NOT be labelled infrastructure. A wrong INFRA
// label is how a regression disappears, and on this tier it would disappear
// across the whole suite at once, since every test starts here. The symmetric
// error costs a person one look at a log, so that is the direction to be wrong
// in: an entry point that forgets this defer reports TEST-FAILURE.
//
// A t.Skipf leaves the region early too and must NOT be declared — a skip
// asserted nothing but also failed nothing — which is why the condition is
// t.Failed() and not "did we reach the end". No harness entry point skips any
// more (NIM-533 made the keeper-binary pre-flight fatal, because a tier that
// skips everything reports `ok` and is read as a pass), but a test parked with
// t.Skip while its rewrite lands still unwinds through here, and it must leave
// no marker behind.
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
