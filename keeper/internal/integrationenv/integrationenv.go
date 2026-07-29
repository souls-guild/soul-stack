// Package integrationenv holds the ONE decision every integration suite asks at
// startup: when the containers this suite needs cannot be brought up, is that a
// failure or a skip?
//
// It used to be answered in ~35 identical copies of a `requireDocker()` helper,
// one per package, and all of them answered it the wrong way round: the suite
// ran only if SOUL_STACK_INTEGRATION_REQUIRE_DOCKER was set, and otherwise
// SKIPPED — with exit code 0. So `go test -tags=integration ./...` on a machine
// without docker, or with the variable simply forgotten, printed a green result
// for a suite that had not executed a single assertion.
//
// That is not a hypothetical. It is how the L1 suite rotted for months (NIM-207
// found tag-guarded packages that had stopped COMPILING; NIM-221 cleaned out the
// backlog that accumulated meanwhile) — a green run reported nothing, so nobody
// looked. NIM-238 is about the trap itself rather than that backlog.
//
// The default is therefore inverted here: passing `-tags=integration` is already
// the statement "I want the integration suites", so this package takes it at its
// word. Missing docker is a FAILURE. Skipping is still available, but it now has
// to be said out loud — and saying it out loud is the point, because a skip is a
// claim that nothing was verified.
package integrationenv

import "os"

// SkipEnv is the opt-OUT: set it when the environment genuinely has no docker and
// a skip is the honest outcome (a laptop without a daemon, a sandbox, a
// docs-only CI lane). Anything other than the empty string counts.
const SkipEnv = "SOUL_STACK_INTEGRATION_SKIP_DOCKER"

// RequireEnv is the legacy opt-IN. It is still honoured — `make test-integration`
// and the CI job both set it, and there is no reason to break a caller that says
// "yes, require docker" — but it is no longer what MAKES the suite run. Setting
// it to a false-looking value does NOT turn requirement off; use [SkipEnv] for
// that, so that "I am skipping" is never expressed as an absent variable.
const RequireEnv = "SOUL_STACK_INTEGRATION_REQUIRE_DOCKER"

// RequireDocker reports whether a container-setup failure must fail the suite
// rather than skip it. True unless [SkipEnv] is set.
//
// Note what is deliberately NOT here: a docker-availability probe. This answers
// the policy question only ("is a failure to start containers fatal?"); whether
// they actually start is the caller's own setup, and conflating the two is how a
// probe that quietly returns false becomes another silent skip.
func RequireDocker() bool {
	return os.Getenv(SkipEnv) == ""
}
