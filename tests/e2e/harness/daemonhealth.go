//go:build e2e

// Telling a contended docker daemon apart from a stand that genuinely failed.
//
// NIM-533. L3a's remaining red, after NIM-469 made the red legible, reads:
//
//	redis: retries: 1065, get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/…":
//	context deadline exceeded
//
// Nothing in that sentence is about redis. `get state` is testcontainers polling
// the daemon for container state; the retry count is how many times it asked;
// the deadline is the wait budget running out while it asked. The container may
// have been listening on its port the whole time. So the tier's remaining red is
// a fact about the machine wearing a stand's name — and a gate whose red cannot
// be attributed is not a gate, which is what made this ticket high.
//
// Two separate things are wrong there, and only one of them is the daemon.
//
// The first is that the harness DROPPED the container. testcontainers returns a
// live handle alongside the error — generic.go says so in as many words: "At
// this point `c` might not be nil. Give the caller an opportunity to call
// Destroy on the container." Every start* here checked the error first and
// returned, so the handle was never registered for teardown. A failed stand
// therefore leaked one to three containers that only ryuk would ever reap, and
// only at process exit — while the suite kept running forty more tests against a
// daemon carrying them. That is a positive-feedback loop, and it is enough on
// its own to explain the tier's actual signature: two or three failures per run,
// a different two or three each time, never reproducible alone. Fixing it is
// adoptContainer below, and it belongs to the daemon's story because it is what
// made the daemon slow.
//
// The second is that when a bring-up does fail, the harness knows things that
// error string does not: whether the daemon answers a trivial call right now and
// how fast, whether the container is running or exited and with what code, and
// what the container last printed. Reporting them is what turns "read this
// testcontainers error and guess" into a statement about which layer failed.
//
// The third thing is that none of the above helps if the daemon stops answering
// at all, and that case was found by running this tier against one. A wedged
// daemon — the socket accepts the connection and no reply ever comes — did not
// produce any verdict at all: the run sat in its FIRST docker call for the whole
// eight-minute test timeout and died as `panic: test timed out`, naming no
// layer. The reason is upstream and is not a bug so much as a shape worth
// knowing: testcontainers resolves the docker host through
//
//	func ExtractDockerHost(ctx context.Context) (string, error) {
//	    dockerHostOnce.Do(func() { dockerHostCache, … = extractDockerHost(ctx) })
//
// (internal/core/docker_host.go), and the only caller that matters,
// NewDockerProvider, passes context.Background() into it (provider.go:144). So
// the first docker call in a process makes an UNBOUNDED /info request, and every
// later one blocks on a sync.Once that the first is still inside. No deadline
// any caller supplies applies to either. That is why this file races its probe
// against a timer instead of bounding it with a context, and why the probe runs
// BEFORE the first container rather than only after a failure.
//
// What this file deliberately does NOT do is retry a test, or relax a wait. The
// one retry it allows is on container bring-up, it is capped at one, it is
// permitted ONLY when the daemon probe says the daemon itself was unresponsive,
// and it PRINTS. A retry nobody can see is how a flaky tier looks healthy; a
// retry that announces itself and is counted at the end of the run is a
// measurement of how often the machine hiccups.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// daemonProbeBudget — how long the probe waits for a trivial daemon call.
//
// Its own budget, on its own context, deliberately: the probe runs at the moment
// a bring-up failed, and the bring-up context is usually the thing that just
// expired. Probing on that context would return "context deadline exceeded"
// instantly for every failure and the verdict would read "contended" always —
// a diagnosis that is never wrong and therefore never says anything.
const daemonProbeBudget = 15 * time.Second

// daemonWarmPing — what an unloaded daemon costs for one round trip.
//
// Measured on the development box, idle: `docker inspect` answers in 30–40 ms,
// `/info` in the same order. A second is over an order of magnitude above that,
// so this does not fire on ordinary jitter; it fires when the daemon has stopped
// keeping up. Above it the harness says the machine was busy, below it the
// harness says the container failed and means it.
const daemonWarmPing = 1 * time.Second

// daemonIdlePing — what an idle daemon actually costs, as measured.
//
// Reported to the reader so "the machine is saturated" comes with the number it
// is being compared against. Its own constant rather than a divisor of
// daemonWarmPing: the two are independent facts, and deriving one from the other
// meant that moving the threshold silently rewrote the measurement.
const daemonIdlePing = 40 * time.Millisecond

// standBringUpAttempts — how many times ONE stand may be started.
//
// Two, not three, and not configurable. The point of the extra attempt is not to
// get the suite green; it is that "the daemon was unresponsive, and it was still
// unresponsive a moment later" is a much stronger claim than a single sample,
// and it is the claim a person needs to decide whether to look at the machine or
// at the code. A second attempt buys that claim for the cost of one bring-up.
//
// The attempt is spent only on a contended verdict, so a container that is
// genuinely broken — bad image, bad config, exits immediately — still fails on
// the first try at full speed. See bringUpStand.
const standBringUpAttempts = 2

// daemonVerdict — the state of the docker daemon at one instant, as measured
// rather than inferred.
type daemonVerdict struct {
	ping time.Duration
	err  error
}

// contended reports whether the daemon, not the container, is the thing that
// looks wrong. Either it did not answer at all, or it answered far slower than
// an idle daemon does.
func (v daemonVerdict) contended() bool {
	return v.err != nil || v.ping > daemonWarmPing
}

// unresponsive reports whether the daemon gave no answer at all, as opposed to a
// slow one.
//
// Strictly narrower than contended, and the two are not interchangeable. The
// pre-flight check in bringUpStand refuses to start a stand on this predicate,
// and refusing on contended() instead would ground the tier on any box that is
// merely loaded — which is most CI boxes, most of the time, and is precisely the
// condition the retry above exists to ride out rather than to abort on. Slow is
// a reason to try; silent is a reason not to.
func (v daemonVerdict) unresponsive() bool {
	return v.err != nil
}

// silent reports whether the daemon gave no answer at all — the timer fired, or
// an earlier probe already established that nothing in this process will get one.
//
// Strictly narrower than unresponsive(), and the difference is the whole point
// of having both. A probe that returns an error has ANSWERED: no socket, wrong
// DOCKER_HOST, no permission on it — a fact about the setup, available in
// milliseconds. Both stop a stand from being attempted, so both refuse; they
// must not share prose, because sending someone to rebuild WSL Integration over
// a typo in DOCKER_HOST is worse than saying nothing at all.
func (v daemonVerdict) silent() bool {
	return errors.Is(v.err, errDaemonUnresponsive) || errors.Is(v.err, errDaemonWedgedEarlier)
}

func (v daemonVerdict) String() string {
	switch {
	case errors.Is(v.err, errDaemonWedgedEarlier):
		return fmt.Sprintf("docker is unusable for the rest of this process: %v", v.err)
	case errors.Is(v.err, errDaemonUnresponsive):
		return fmt.Sprintf("docker daemon did not answer within %s: %v", daemonProbeBudget, v.err)
	case v.err != nil:
		// Answered, and the answer was an error. Reporting the elapsed time
		// matters here: it is the number that separates this from silence, and
		// stating "did not answer within 15s" over a 3 ms refusal is the lie
		// this branch exists to prevent.
		return fmt.Sprintf("docker daemon refused the call after %s: %v",
			v.ping.Round(time.Millisecond), v.err)
	case v.ping > daemonWarmPing:
		return fmt.Sprintf("docker daemon answered in %s (an idle one answers in ~%s) — the machine is saturated",
			v.ping.Round(time.Millisecond), daemonIdlePing)
	default:
		return fmt.Sprintf("docker daemon answered in %s — it is healthy", v.ping.Round(time.Millisecond))
	}
}

// refusalGuidance is the layer paragraph for a stand that was never attempted.
//
// Split from the caller so the two shapes cannot share a sentence by accident:
// the pre-flight refuses on unresponsive(), which is the union, and only this
// function knows which half it is looking at.
func (v daemonVerdict) refusalGuidance(name string) string {
	if !v.silent() {
		return fmt.Sprintf("  → read this as the MACHINE's SETUP: the daemon answered, and what it "+
			"answered was an error, so this is not a daemon that is slow or wedged — docker is not "+
			"usable from this process at all. Check DOCKER_HOST, the socket path, and permission on "+
			"it. Nothing belonging to %s has run, so nothing here is a finding about the code or "+
			"the image.", name)
	}
	return fmt.Sprintf("  the harness asked the daemon for a trivial /_ping before raising anything, "+
		"and did not get an answer in %s"+
		"\n  → read this as the MACHINE: nothing belonging to %s has run, so nothing here is a "+
		"finding about the code or the image. The shape is a docker socket that accepts "+
		"connections and never replies; under WSL2 that is the Docker Desktop relay after a "+
		"restart, and recreating the distro's WSL Integration clears it."+
		"\n  Without this check the first container call would wait inside that unanswered request "+
		"until `go test -timeout` killed the binary, and the run would be reported as a TIMEOUT "+
		"naming no layer at all (NIM-533).", daemonProbeBudget, name)
}

// errDaemonUnresponsive — the daemon accepted the connection and then said
// nothing.
//
// Its own error, not a message, because the two ways the probe can fail mean
// opposite things. A provider that returns an error has ANSWERED: no socket,
// wrong DOCKER_HOST, permissions — a fact about the setup, available in
// milliseconds. This one is the shape where the connection succeeds and no reply
// ever comes, which is the shape that costs a whole test timeout and reports
// nothing. Only the second says the process is finished with docker.
var errDaemonUnresponsive = errors.New(
	"the socket accepted the connection and no reply ever came")

// errDaemonWedgedEarlier — a probe in this process hung where no deadline reaches.
var errDaemonWedgedEarlier = errors.New(
	"an earlier probe in this run hung inside a docker call that no deadline reaches — " +
		"testcontainers resolves the docker host inside a sync.Once and makes its first Info " +
		"call with context.Background(), and neither takes ours. That call has not returned, " +
		"so a later probe would hang in the same place for the same budget")

// daemonWedged records that a probe hung in a call its budget could not stop.
//
// Set only for that case, and this is the distinction the whole latch stands on.
// probeDaemonWithin cannot end the call it races — it can only stop waiting — so
// the verdict it returns says nothing about whether the call is still running.
// The two shapes differ in a way that is observable: everything inside
// NewDockerProvider ignores the context it is given, while the Ping after it is
// bound by one. A timeout before the provider exists therefore means the call is
// parked somewhere a deadline never arrives, and every later probe would park in
// exactly the same place for the full budget — a forty-test tier spending ten
// minutes to rediscover one fact. A timeout after it means the daemon merely did
// not answer a ping in time; that call ends on its own, the next one is free to
// find a recovered daemon, and latching there would ground the tier on one slow
// sample while claiming docker is unusable — confidently naming the wrong layer,
// which is the failure this file exists to stop.
//
// Cleared by the marker below as well as set here: a lookup that completes is
// the fact any earlier latch was standing on, and once it is gone the latch has
// nothing left to assert.
var daemonWedged atomic.Bool

// dockerPing is the call the probe races: a /_ping round trip on the same client
// stack the containers use, so this measures the hop they are failing on rather
// than a proxy for it.
//
// Ping and NOT Health, which is the obvious choice and is wrong. Health is
// `client.Info`, and testcontainers memoises Info in package variables —
// dockerInfo/dockerInfoSet/dockerInfoLock in docker_client.go — returning the
// cached value with no I/O after the first success. A probe built on Health
// therefore stops touching the daemon as soon as one call succeeds: on a healthy
// box the first stand warms the cache and every later probe reports a
// microsecond ping and a nil error no matter what the daemon is doing. That
// makes contended() permanently false (so the retry below is dead code), makes
// this file's pre-flight unable to catch a daemon that dies mid-run, and makes
// describeStandFailure name the CONTAINER for a saturated daemon — confidently
// naming the wrong layer, which is worse than the raw library error this file
// replaced. Ping is a plain pass-through to moby's client (docker_client.go
// wraps it without caching) and hits /_ping every call.
//
// This is invisible against a daemon that is wedged from the start, because the
// cache never gets its one success and every probe stays real. The three
// identical runs that accepted NIM-533 were all of that shape, so they could not
// have caught it — which is why the ban is also a guard and not just a comment.
//
// A variable so the guards can substitute a call that never returns. That is the
// exact condition this machinery exists for, and it is not reproducible on
// demand against a real daemon — without the seam, the timeout path and the
// wedged-process path would be the two branches in this file with no known-bad,
// which is the defect NIM-469 spent four rounds on.
// The bounded callback is the probe reporting on itself. Nothing above it can be
// stopped by ctx and everything below it can, so calling it here is what lets
// probeDaemonWithin tell a call it may never get back from apart from one that
// is simply taking too long. It is a parameter rather than a package variable
// because the guards substitute this whole function, and a marker they cannot
// choose to leave unset would make the wedged case unreachable in a test.
var dockerPing = func(ctx context.Context, bounded func()) error {
	// Same constructor GetProvider uses for ProviderDocker, so this enters the
	// same dockerHostOnce the file header describes — the wedge detection below
	// depends on that being the identical path, not a similar one.
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("docker provider: %w", err)
	}
	defer func() { _ = provider.Close() }()

	bounded()

	_, err = provider.Client().Ping(ctx, client.PingOptions{})
	return err
}

// probeDaemon times one trivial call to the docker daemon.
func probeDaemon() daemonVerdict { return probeDaemonWithin(daemonProbeBudget) }

// probeDaemonWithin is probeDaemon with the budget named, so a guard can pick
// one it is willing to wait for.
//
// The docker call runs on its own goroutine and is raced against a timer rather
// than bounded by the context it is given, because the context does not bind it.
// The first docker call in a process resolves the docker host inside a sync.Once
// that testcontainers enters with context.Background() (file header), so against
// a daemon that never replies, that call never returns and no deadline reaches
// it. The timer is the only thing in this process that will end it.
//
// The goroutine is then leaked, on purpose. In the case that matters it is
// holding a Once that nothing can take back, so the alternative is not "clean up
// instead" — it is what this tier did before the race existed, which was to sit
// in that request until `go test -timeout` killed the binary and reported
// TIMEOUT against no layer at all. A leaked goroutine on a run that has already
// lost docker is the smaller harm, and daemonWedged above makes it exactly one.
// In the other case — the ping outran its own deadline — the goroutine is not
// leaked at all, it just finishes after this function stopped listening, which
// is what the channel's buffer is for.
func probeDaemonWithin(budget time.Duration) daemonVerdict {
	if daemonWedged.Load() {
		return daemonVerdict{err: errDaemonWedgedEarlier}
	}

	started := time.Now()
	// Buffered: the goroutine must be able to finish and exit after this
	// function has stopped listening, or a probe that timed out would leak a
	// second goroutine every time it was called.
	done := make(chan daemonVerdict, 1)
	// Resolved here rather than inside the goroutine. The goroutine outlives this
	// call by design, and reading a package variable from something that outlives
	// its caller is a race the moment anything writes it — which the guards do,
	// and -race says so.
	ping := dockerPing
	// Set once the call has reached the part of itself that ctx can stop. Read
	// only after the timer fires, and written by a goroutine that outlives this
	// function — hence atomic.
	var bounded atomic.Bool
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		done <- daemonVerdict{err: ping(ctx, func() {
			bounded.Store(true)
			// In this order, and both stores. Getting here means the docker-host
			// lookup completed, which is the only thing an earlier latch was
			// asserting, so it is now false whoever set it.
			daemonWedged.Store(false)
		})}
	}()

	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case v := <-done:
		v.ping = time.Since(started)
		return v
	case <-timer.C:
		// The clear in the callback is what makes a late completion retract this;
		// a guard covers that. Storing before reading rather than the other way
		// round closes what is left — the interleaving where the callback runs
		// between the read and the store — which no guard here can produce on
		// demand, so it is stated rather than tested.
		daemonWedged.Store(true)
		if bounded.Load() {
			daemonWedged.Store(false)
		}
		return daemonVerdict{ping: time.Since(started), err: errDaemonUnresponsive}
	}
}

// adoptContainer takes ownership of a container handle for teardown.
//
// Called BEFORE the error is checked, which is the whole point. A bring-up that
// fails its wait strategy has usually created and started the container — the
// wait is the last step, not the first — and the library hands that handle back
// with the error rather than cleaning it up, by documented design. Registering
// it here means a red stand costs the daemon nothing after the test that owned
// it ends.
//
// Nil-tolerant on purpose: the modules return a typed pointer, so a caller that
// forwarded a nil one into this interface parameter would produce a non-nil
// interface holding nil and a panic at teardown. Call sites check their concrete
// pointer; this is the second line.
func (s *Stack) adoptContainer(name string, c testcontainers.Container) {
	if c == nil {
		return
	}
	if s.standContainers == nil {
		s.standContainers = map[string]testcontainers.Container{}
	}
	s.standContainers[name] = c
	s.containers = append(s.containers, c)
	s.cleanups = append(s.cleanups, func() {
		// The map is the ownership record: a bring-up retry terminates the
		// failed attempt itself and drops it from the map, and this closure has
		// no other way to know that. Without the check, teardown would call
		// Terminate on a dead handle and log that the container "did not
		// terminate" — a leak report about a container the harness had already
		// cleaned up, which is the one thing terminateContainer's logging exists
		// to make believable.
		if s.standContainers[name] != c {
			return
		}
		s.terminateContainer(name, c)
	})
}

// disownContainer drops a handle the harness has already terminated itself.
//
// Both records, because they answer different questions and a half-updated pair
// is worse than either: the map is what the cleanup closure consults to decide
// whether it still owns the container, and the slice is the stand's inventory of
// what it raised. A retry's second attempt must not inherit the first attempt's
// corpse in either of them.
func (s *Stack) disownContainer(name string) {
	c, ok := s.standContainers[name]
	if !ok {
		return
	}
	delete(s.standContainers, name)
	// Recorded HERE, in the function that erases the evidence, and not in the
	// caller that decided to. This delete is the only thing that can turn "an
	// image ran and failed" into an empty map, so pairing the record with it
	// means a future caller cannot erase a container without saying so — which
	// is what a first version of this got wrong by recording in
	// terminateStandAttempt, leaving the deletion reachable without the note.
	if s.standRetriedAway == nil {
		s.standRetriedAway = map[string]bool{}
	}
	s.standRetriedAway[name] = true

	kept := s.containers[:0]
	for _, held := range s.containers {
		if held != c {
			kept = append(kept, held)
		}
	}
	s.containers = kept
}

// bringUpStand starts one stand, and makes the harness — not the reader — say
// which layer failed when it does not come up.
//
// The retry here is the narrowest one that answers NIM-533's question. It is
// spent only when the daemon probe says the daemon was unresponsive at the
// moment of failure, so a container that is actually broken is not retried at
// all; it is capped at standBringUpAttempts; and every attempt past the first is
// printed with the evidence that bought it. The suite's teardown of the failed
// attempt happens first, so the second attempt starts from a clean daemon rather
// than adding to it.
func (s *Stack) bringUpStand(ctx context.Context, name string, start func(context.Context) error) error {
	s.t.Helper()

	// Before raising anything, not only after something failed.
	//
	// A daemon that never replies is the one failure the post-mortem probe below
	// cannot report, because the bring-up it is meant to explain never returns to
	// let it run. Asking first, on a budget that is enforced, converts an
	// unattributed `panic: test timed out after 8m0s` into a named refusal in
	// fifteen seconds. Here rather than in the constructors because every stand
	// comes up through this function — a guard checks that — so there is no call
	// site to add and none to forget.
	//
	// On unresponsive() and not contended(): a slow daemon still gets its stand.
	if v := probeDaemon(); v.unresponsive() {
		return fmt.Errorf("%s stand was not attempted: %s\n%s", name, v, v.refusalGuidance(name))
	}

	var lastErr error
	var lastVerdict daemonVerdict
	for attempt := 1; attempt <= standBringUpAttempts; attempt++ {
		err := start(ctx)
		if err == nil {
			if attempt > 1 {
				s.t.Logf("[stand-retry] %s came up on attempt %d of %d. The first attempt failed "+
					"because %s — this run's result is real, but the machine was the reason it "+
					"nearly was not.", name, attempt, standBringUpAttempts, lastVerdict)
			}
			return nil
		}
		lastErr, lastVerdict = err, probeDaemon()

		// A healthy daemon that still could not raise the container is a finding
		// about the container. Do not spend an attempt disguising it.
		if !lastVerdict.contended() {
			break
		}
		if attempt == standBringUpAttempts {
			break
		}
		if ctx.Err() != nil {
			// No budget left to retry into; a second attempt would fail
			// instantly and the log would blame the container for it.
			break
		}
		s.t.Logf("[stand-retry] %s failed on attempt %d of %d and %s. Tearing that attempt down "+
			"and trying once more.", name, attempt, standBringUpAttempts, lastVerdict)
		s.terminateStandAttempt(name)
	}
	return s.describeStandFailure(ctx, name, lastErr, lastVerdict)
}

// terminateStandAttempt tears down whatever the failed attempt left running, so
// the retry does not simply add a second container to a daemon that is already
// behind.
func (s *Stack) terminateStandAttempt(name string) {
	c, ok := s.standContainers[name]
	if !ok {
		return
	}
	s.terminateContainer(name, c)
	s.disownContainer(name)
}

// describeStandFailure turns a library error into a statement about a layer.
//
// Three facts the caller cannot get from the wrapped error, in the order that
// decides what to do about them: what the daemon was doing at the moment of
// failure, what state the container reached, and what it printed. Any of them
// may be unavailable — a daemon too busy to answer the bring-up is often too
// busy to answer these either — and each says so in place rather than being
// dropped, because "could not read the container's state" is itself evidence
// about the daemon.
func (s *Stack) describeStandFailure(ctx context.Context, name string, cause error, v daemonVerdict) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s stand did not come up: %v", name, cause)
	fmt.Fprintf(&b, "\n  daemon at the moment of failure: %s", v)

	if parent := ctx.Err(); parent != nil {
		fmt.Fprintf(&b, "\n  the shared bring-up context was already %v, so this stand did not get "+
			"its own %s — an earlier stand spent the budget", parent, standReadyTimeout)
	}

	c, ok := s.standContainers[name]
	switch {
	case ok:
		fmt.Fprintf(&b, "\n  container: %s", describeContainerState(c))
		if tail := containerLogTail(c, 12); tail != "" {
			fmt.Fprintf(&b, "\n  last lines it printed:\n%s", indentLines(tail, "    | "))
		}
	case s.standRetriedAway[name]:
		// Not "never created". An earlier attempt DID run, and this harness is
		// what removed it — saying otherwise sends the reader looking for an
		// image-pull failure that never happened.
		b.WriteString("\n  container: an earlier attempt did run and was torn down by the retry, " +
			"and the attempt after it failed before a handle existed — so the logs that would " +
			"explain this are gone, deleted by the retry. Rerun with the retry exhausted (one " +
			"stand, idle box) to keep them")
	default:
		b.WriteString("\n  container: never created, so the failure is before the image ran " +
			"(image pull, daemon refusing the create, or the context expiring first)")
	}

	switch {
	case isReaperFailure(cause):
		fmt.Fprintf(&b, "\n  → read this as the DEPENDENCY: what did not come up is testcontainers' own "+
			"reaper (ryuk), not %s's image. Nothing here configures it — its readiness wait is "+
			"hardcoded to 60s in testcontainers-go (reaper.go, no WithStartupTimeout, no env "+
			"override), so neither %s nor any budget in this harness applies. See NIM-532. "+
			"Reading this as a finding about %s is the mistake this line exists to stop.",
			name, standReadyTimeout, name)
		if v.contended() {
			// Both, not one. ryuk's 60s wait is what a saturated daemon tips over
			// first, and testcontainers wraps only some paths with "reaper: " — so
			// on one contended run the same root cause reaches this function as
			// DEPENDENCY for one stand and MACHINE for its neighbour. Naming the
			// contention here is what lets a reader put those two reports together
			// instead of chasing ryuk on a box that was simply out of capacity.
			fmt.Fprintf(&b, "\n     and the daemon was ALSO not keeping up (%s) — on a saturated box "+
				"ryuk's 60s wait is usually the first thing to tip over, so treat the machine as "+
				"the root and the reaper as where it surfaced.", v)
		}
	case v.contended():
		fmt.Fprintf(&b, "\n  → read this as the MACHINE, not the code: the daemon was not keeping up, "+
			"and %d attempt(s) were spent confirming that. Rerunning this test alone on an idle "+
			"box is the check that distinguishes it from a real regression.", standBringUpAttempts)
	default:
		b.WriteString("\n  → read this as the CONTAINER: the daemon was responsive, so the image or " +
			"its configuration is what did not become ready. This one does not go away on a rerun.")
	}
	return errors.New(b.String())
}

// reaperFailurePrefix — how testcontainers wraps every failure to obtain its own
// reaper. docker.go puts this in front of the lookup, the create and the connect
// paths alike, so one substring covers all three; the inner text ("new reaper:",
// "from container …:") varies and is not worth matching on.
const reaperFailurePrefix = "reaper: "

// isReaperFailure reports whether the stand failed because the reaper did, which
// is a statement about the dependency and not about this stand.
//
// It is checked BEFORE the daemon verdict, and it has to be: ryuk's wait is 60s,
// so probeDaemon() runs a full minute after the contention that caused it and
// frequently finds the daemon idle again. The verdict would then say "the image
// or its configuration", naming a layer that was never involved.
func isReaperFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), reaperFailurePrefix)
}

// describeContainerState reads the container's state on a fresh, short context.
// Fresh because the bring-up context is normally spent by now; short because a
// daemon that will not answer in a couple of seconds has already told us what we
// were asking.
func describeContainerState(c testcontainers.Container) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := c.State(ctx)
	if err != nil {
		return fmt.Sprintf("state unreadable (%v) — the daemon would not answer for it either", err)
	}
	desc := fmt.Sprintf("status=%s running=%t exitcode=%d", st.Status, st.Running, st.ExitCode)
	if st.Error != "" {
		desc += " error=" + st.Error
	}
	if st.OOMKilled {
		desc += " OOMKilled=true (the box ran out of memory, which is the machine again)"
	}
	return desc
}

// containerLogTail returns the last n lines the container printed.
//
// Capped by bytes as well as lines: a container that fails after logging for a
// minute would otherwise bury the failure it is supposed to explain.
//
// Control bytes are stripped for carriage returns and ANSI escapes, which
// progress-printing entrypoints emit freely and which turn a log tail into
// overstruck noise. NOT for docker's multiplexing frame headers: Logs() on a
// non-TTY container already de-multiplexes (parseMultiplexedLogs in docker.go),
// so they never reach here — and a rune filter would not remove them anyway,
// since a frame's 4-byte length field is printable for any line of 32–126 bytes,
// which is most of them.
func containerLogTail(c testcontainers.Container, n int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc, err := c.Logs(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()

	const maxBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(rc, maxBytes))
	if err != nil && len(raw) == 0 {
		return ""
	}

	var clean bytes.Buffer
	for _, r := range string(raw) {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			clean.WriteRune(r)
		}
	}
	lines := strings.Split(strings.TrimRight(clean.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	return out
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
