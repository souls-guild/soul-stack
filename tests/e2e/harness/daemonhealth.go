//go:build e2e

// Telling a contended docker daemon apart from a stand that genuinely failed.
//
// NIM-533. L3a's remaining red, after NIM-469 made the red legible, reads:
//
//	redis: retries: 1065, get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/…":
//	context deadline exceeded
//
// Nothing in that sentence is about redis's code. `get state` is testcontainers
// polling the daemon for container state; the retry count is how many times it
// asked; the deadline is the wait budget running out while it asked. So the
// tier's remaining red is a fact about the machine wearing a stand's name — and a
// gate whose red cannot be attributed is not a gate, which is what made this
// ticket high.
//
// NIM-646 corrects one sentence that used to stand here: "the container may have
// been listening on its port the whole time". It may not, and the count does not
// say what it looks like it says. This ticket's own live run captured the
// unabridged form:
//
//	postgres container: run postgres: wait until ready: external check:
//	check target: retries: 977 address: localhost:34142: get state:
//	Get "http://…/containers/<id>/json": context deadline exceeded
//
// In that form each of those 977 rounds ANSWERED its inspection and then found
// the published address REFUSED, so the number says the opposite of a stalled
// daemon: it was keeping up, and the port was shut. Which half of the string gets
// read is the whole misreading of this tier; see waitRetries below. The
// `address:` is load-bearing — the library has two other `retries:` counters that
// mean different things — and so is everything after it: the SAME envelope
// carries a container that exited, one the kernel killed for memory, and a docker
// call that failed outright, and those get opposite verdicts. The 1065 line at
// the top is quoted as NIM-533 recorded it, which is not any format string this
// library has at that position; treat it as somebody's abridgement, not as
// evidence about which loop printed it.
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
	"regexp"
	"strconv"
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
// The attempt is spent only where the evidence says this box delayed the stand,
// so a container that is genuinely broken — bad image, bad config, exits
// immediately — still fails on the first try at full speed. See
// machineDelayedTheStand for which shapes those are and which are excluded.
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
// makes contended() permanently false (which is the probe-side half of the retry
// decision, so only the cause-side shapes could still buy one), makes
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

// machineDelayedTheStand reports whether the evidence says this box, rather than
// the image, is why the stand did not come up — and therefore whether the one
// retry is worth spending.
//
// The post-mortem probe alone is not that evidence, and that is NIM-646's second
// consequence. contended() samples the daemon AFTER the failed attempt was torn
// down, by which time a daemon that spent the whole wait behind a queue has one
// less stand to carry and answers in single-digit milliseconds. Deciding from it
// alone meant the retry built to ride out a loaded box was never spent on one:
// describeStandFailure printed "read this as the MACHINE … rerunning this test
// alone on an idle box is the check" and the loop above it declined to do the
// nearest thing it could. The retry exists to turn one sample into a claim — see
// standBringUpAttempts — so refusing it on the shape that most needs a second
// sample is backwards.
//
// The cause line is the second witness, and unlike the probe it was recorded
// INSIDE the window that failed. Which shape it is cannot be read off any single
// predicate, though: inPortWait is true for every cause carrying the port wait's
// count, and daemonStoppedAnswering only means the daemon on causes the branches
// ABOVE it have already declined. Their verdicts come from that order, so this
// reproduces the order rather than the predicates — the exclusions below are the
// switch's earlier cases, and a guard fails if the two lists drift apart.
//
// What remains is the two shapes that say this box was slow: a docker call that
// failed outright mid-wait, and a count too small for the budget it burned. Both
// are conditions a second attempt can clear, and the harness already tells the
// reader so.
//
// The outside-actor shapes are excluded on purpose, and not out of squeamishness.
// An OOM kill, a leftover reaper, a prune and a hand at a terminal all say
// something else is acting on this daemon right now; a second attempt walks the
// same stand straight back into it, and under memory pressure it does so by
// asking for the memory the box has just proved it does not have. Those branches
// ask for a rerun of the test ALONE, which is a different remedy from a second
// attempt inside a run that is already losing. waitOutlivedItsBudget is excluded
// for the opposite reason: it is the one shape whose own arithmetic says the
// daemon was answering throughout, so there is no machine delay to ride out.
func machineDelayedTheStand(cause error, v daemonVerdict) bool {
	// Unchanged from before NIM-646, and deliberately ahead of the exclusions:
	// a daemon measurably struggling right now buys its retry whatever the
	// cause says. Everything below only ADDS shapes to that.
	if v.contended() {
		return true
	}
	// Every arm describeStandFailure has, in the order it has them, and a default
	// that grants nothing. Written the long way on purpose: a shorter "everything
	// the exclusions did not catch" reads the same today, but a branch added to
	// the report between these would fall into that default and buy itself a
	// retry with nothing here having to change — the failure mode where the code
	// stays silent is the one worth spending lines on. A guard holds this list
	// against the report's.
	switch {
	case isReaperFailure(cause), outOfMemory(cause), killedBySignal(cause),
		beingRemoved(cause), containerGone(cause), containerVanished(cause):
		return false
	case daemonStoppedAnswering(cause):
		return true
	case waitOutlivedItsBudget(cause):
		return false
	case inPortWait(cause):
		return true
	default:
		return false
	}
}

// retryWitness names which of the two witnesses bought this attempt, in that
// witness's own words.
//
// machineDelayedTheStand reads two independent pieces of evidence, and they can
// disagree: a docker call that died inside the wait leaves a healthy probe
// behind, because the daemon recovers in the second it takes to ask it. Printing
// the probe alone in that case reports "it is healthy" as the reason a retry was
// spent — a sentence that argues against the decision it is explaining. So print
// the one that actually decided.
func retryWitness(cause error, v daemonVerdict) string {
	if v.contended() {
		return fmt.Sprintf("the probe taken right after it failed: %s", v)
	}
	return fmt.Sprintf("the cause, measured inside the window that failed (the probe a moment "+
		"later said %s, which is why it is not the witness here): %v", v, cause)
}

// bringUpStand starts one stand, and makes the harness — not the reader — say
// which layer failed when it does not come up.
//
// The retry here is the narrowest one that answers NIM-533's question. It is
// spent only when the evidence — the probe at the moment of failure OR the cause
// line recorded inside the window that failed — says this box delayed the stand,
// so a container that is actually broken is not retried at all; it is capped at
// standBringUpAttempts; and every attempt past the first is printed with the
// evidence that bought it. The suite's teardown of the failed
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
				s.t.Logf("[stand-retry] %s came up on attempt %d of %d. The first attempt failed, "+
					"and the retry was bought by %s — this run's result is real, but this box is "+
					"why it nearly was not.", name, attempt, standBringUpAttempts,
					retryWitness(lastErr, lastVerdict))
			}
			return nil
		}
		lastErr, lastVerdict = err, probeDaemon()

		// A box that was not the reason is a finding about the container. Do not
		// spend an attempt disguising it.
		if !machineDelayedTheStand(lastErr, lastVerdict) {
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
		s.t.Logf("[stand-retry] %s failed on attempt %d of %d, and the retry was bought by %s. "+
			"Tearing that attempt down and trying once more.", name, attempt, standBringUpAttempts,
			retryWitness(lastErr, lastVerdict))
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

	var tailPrinted bool
	c, ok := s.standContainers[name]
	switch {
	case ok:
		fmt.Fprintf(&b, "\n  container: %s", describeContainerState(c))
		if tail := containerLogTail(c, 12); tail != "" {
			fmt.Fprintf(&b, "\n  last lines it printed:\n%s", indentLines(tail, "    | "))
			tailPrinted = true
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
	case outOfMemory(cause):
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: the wait stopped because the kernel killed "+
			"the container for memory. That is this box's capacity and not %s's image, and it "+
			"arrives inside the same wait report as a container that was merely slow — with the "+
			"opposite verdict. Rerunning this test alone on an idle box is the check.", name)
	case killedBySignal(cause):
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: docker reports %d, and 128+N is what a "+
			"process that died on signal N reports. docker cannot tell you that is what happened "+
			"— a program free to call exit(137) reports the same integer — but no service image "+
			"chooses to end that way, and SIGKILL and SIGTERM are exactly what docker's own kill "+
			"and stop paths send. Nothing in this harness signals a stand while that stand's own "+
			"wait is still running, so this came from outside the run: the kernel's OOM killer "+
			"picking this process without docker recording it as an OOM kill (the branch above "+
			"catches the case where it did), a `docker kill` or `docker stop` aimed at this "+
			"container by hand, or a kill on the host against its process. A prune is NOT on that "+
			"list, and a reaper almost certainly is not either, for two different reasons: a prune "+
			"never touches a running container at all (moby daemon/prune.go skips one under `if "+
			"!c.IsRunning()` and asks "+
			"for the rest with force unset), and a reaper reaches neither reading whichever way "+
			"it asks for the REMOVAL: without force it is refused outright on a running "+
			"container, and WITH force the removing flag is already up by the time it kills (moby "+
			"daemon/delete.go sets it in containerRm, above the kill in cleanupContainer) — so it "+
			"surfaces at the branch below rather than as a code here. That it asks for a removal "+
			"at all, rather than signalling the container itself, is the weakest link here. The "+
			"docs that ship inside testcontainers-go (docs/features/garbage_collector.md) say the "+
			"reaper REMOVES what it reaps, which is the reading used above; the next paragraph of "+
			"that same page says a reaped container is KILLED, without naming what kills it. "+
			"Neither settles it at the precision this paragraph needs — moby-ryuk's own source "+
			"would, and it is not a Go dependency here. That is not %s's image. Rerun this test "+
			"alone.",
			exitCode(cause), name)
	case beingRemoved(cause):
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: docker did not report a container that "+
			"stopped, it reported one it is in the middle of REMOVING (status %q). A container "+
			"reaches that status only because something called remove on it, and nothing in this "+
			"harness removes a stand while that stand's own wait is still running — so a reaper "+
			"left over from an earlier run, a prune, or a hand at a terminal did. That is not "+
			"%s's image. This is the ONE sample in which a remover is visible carrying state: it "+
			"raises the removing flag before it kills, so a moment later the same actor arrives "+
			"as a container that is already gone, and there is no moment earlier at which it "+
			"arrives as an exit code. Rerun this test alone.",
			containerStatus(cause), name)
	case containerGone(cause):
		// The tail is named only when there IS one, for the same reason the budget
		// branch below forks on it. This branch is reached with no container handle
		// at all — the retry deletes it (disownContainer), and then the report says
		// "the logs that would explain this are gone" six lines before pointing at
		// them. Evidence claimed and absent costs a reader more than evidence not
		// claimed, and this arm was the one place in the switch still doing it.
		b.WriteString("\n  → read this as the CONTAINER: the wait did not run out of time — an " +
			"inspection found the container no longer running, and the cause above carries the " +
			"exit code or status it reported. What that cannot tell you is whether the container " +
			"CHOSE it. docker reports one integer for a program that returned on its own and for " +
			"one something else stopped, and a plain `docker stop` does not even reach these " +
			"three stands the same way: postgres:16-alpine declares STOPSIGNAL SIGINT, while " +
			"redis and vault carry no STOPSIGNAL and take the SIGTERM default — and what each " +
			"image then reports is up to its handler, which is the whole point.")
		if tailPrinted {
			b.WriteString(" The log tail above separates the two readings — an image that failed " +
				"says so before it goes, and a container stopped mid-startup just stops.")
		} else {
			b.WriteString(" A log tail is what would separate the two readings, and none was " +
				"captured here, so this report cannot make that separation for you.")
		}
		fmt.Fprintf(&b, " Rerun this test alone: passing alone means something outside this run "+
			"ended it, failing alone means %s.", name)
	case containerVanished(cause):
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: %d inspection(s) into the readiness wait, the "+
			"daemon answered that there is no such container. Nothing in this harness removes a "+
			"container while that container's own wait is still running, so something outside this "+
			"run did — a reaper left over from an earlier run, a prune, or a daemon restart. That "+
			"is not %s's image, and it is not a daemon that stopped answering either: this call was "+
			"answered, and the answer was that the container is gone. Rerun this test alone.",
			waitRetries(cause), name)
	case daemonStoppedAnswering(cause):
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: %d inspection(s) into the readiness wait, one "+
			"came back with no container state at all — the docker call itself failed, and the cause "+
			"above says how. The daemon line at the top of this report was measured after that, by "+
			"which time it had one less stand to carry. Rerunning this test alone on an idle box is "+
			"the check.", waitRetries(cause))
	case waitOutlivedItsBudget(cause):
		// The container, but NOT "therefore the image". This is the branch three
		// live runs landed in while the stands they condemned passed alone in
		// 9.5-58s, and what made it wrong was the sentence after the verdict, not
		// the verdict: a container that is up and merely starved of CPU is
		// indistinguishable from a broken one on this evidence, and only one of
		// the two survives a rerun. Say what is known and name the check.
		//
		// The count is printed HERE and not above the switch on purpose. It is a
		// measurement of one specific loop, and the branches around it describe
		// other things entirely — a ryuk failure's dials went to ryuk's address,
		// not to this stand's, and a MACHINE verdict would be handed a line saying
		// the daemon ANSWERED every inspection right before being told it was not
		// keeping up.
		//
		// "published address", not "the container's port". externalCheck dials
		// target.Host() plus the MAPPED port (wait/host_port.go:205-232), so a
		// publish path that does not carry the port on this box refuses exactly
		// like an image that never listened. On postgres it is also known that the
		// server DID listen: ForAll runs strategies in sequence (wait/all.go:90-111)
		// and its log wait (occurrence 2) precedes the port wait, so the second
		// "ready to accept connections" was already in the log before the first
		// refused dial. redis and vault put the port first, so there the same
		// sentence is not available — which is why the report points at the tail
		// instead of naming a culprit.
		n := waitRetries(cause)
		slept := (time.Duration(n) * waitPollInterval).Round(time.Second)
		fmt.Fprintf(&b, "\n  the readiness wait recorded %d container inspections the daemon ANSWERED "+
			"and %d dials this box's published address REFUSED. Its own sleeps come to %s of the %s "+
			"this stand was given, so those inspections, those dials and whatever ran before this "+
			"wait all fit in the %s that is left — and unlike the daemon line above, every bit of "+
			"that was measured inside the window that failed",
			n, n, slept, standReadyTimeout, standReadyTimeout-slept)
		fmt.Fprintf(&b, "\n  → read this as the CONTAINER: the daemon answered every one of those "+
			"inspections at that rate, so it was not the thing that was stuck — what did not happen "+
			"in time is %s's published address accepting a connection. Three things land here "+
			"identically: an image that never listened, a container too starved to get there, and a "+
			"publish path on this box that did not carry the port.", name)
		if tailPrinted {
			b.WriteString(" The log tail above separates the first — a service that already " +
				"announced itself ready did its part.")
		} else {
			b.WriteString(" Nothing was captured from the container, so the tail that would " +
				"separate the first of them is not available here.")
		}
		fmt.Fprintf(&b, " For the rest, rerun this test alone: passing alone means the machine, "+
			"failing alone means %s.", name)
	case inPortWait(cause):
		// The same envelope and the same kind of count as the branch above, and the
		// opposite verdict — because the count is a numerator and the budget is the
		// denominator. Ten answered inspections inside a spent two minutes is not a
		// daemon keeping up; it is one taking about ten seconds per call. Reading
		// the numerator alone is what put NIM-533's misattribution here in the
		// first place, so it is stated as the arithmetic rather than as a verdict.
		n := waitRetries(cause)
		slept := (time.Duration(n) * waitPollInterval).Round(time.Second)
		fmt.Fprintf(&b, "\n  → read this as the MACHINE: the readiness wait spent the whole %s it was "+
			"given, and its own sleeps come to only %s of that (%d refused dials, %s apart), so the "+
			"remaining %s went into the %d docker inspections it made. The calls themselves are what "+
			"ran slowly, and a probe taken after the stand was torn down — the daemon line at the "+
			"top — cannot see that. Rerunning this test alone on an idle box is the check.",
			standReadyTimeout, slept, n, waitPollInterval, standReadyTimeout-slept, n+1)
	default:
		b.WriteString("\n  → read this as the CONTAINER: nothing in the cause carries the port wait's " +
			"count, so what failed is not that wait running out of time but the image reaching the " +
			"state this stand waits for. Rerun this test alone to tell a broken image from a box " +
			"that could not get it there: passing alone means the machine.")
	}
	return errors.New(b.String())
}

// waitRetries — how many answered inspections the port wait recorded before it
// gave up, and -1 when the cause is not that wait.
//
// Not decoration, and the reason this file no longer decides from the probe
// alone. testcontainers' externalCheck asks the daemon for the container's state
// at the top of every iteration and dials the mapped port after it, sleeping
// 100ms on a refused connection (wait/host_port.go:256-282). So `retries: 977`
// is 977 docker inspections that were ANSWERED and 977 dials that were REFUSED,
// counted inside the window that failed — which is exactly what the post-mortem
// probe cannot supply and what NIM-646's misattribution came from. (The loop ran
// once more than that: iteration 977 is the one whose inspection failed, and its
// error is what the cause echoes.)
//
// It also settles the shape that looks most like a stalled daemon and is not
// one: the wait exits THROUGH a docker call, so a container that never opens its
// port ends the run with `get state: Get ".../containers/<id>/json": context
// deadline exceeded` — a docker API request that ran out of time, on a daemon
// that had just answered a thousand of them. Reading that string as a fact about
// the daemon inverts the misattribution instead of fixing it.
func waitRetries(cause error) int {
	if cause == nil {
		return -1
	}
	m := waitRetryCount.FindStringSubmatch(cause.Error())
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

// waitRetryCount — externalCheck's count, and deliberately ONLY that one.
//
// Anchoring here is not caution, it is the difference between a true sentence and
// a false one. wait/host_port.go prints "retries: %d" from three loops and they do
// not count the same thing:
//
//	:263  check target: retries: N address: …   externalCheck — N answered
//	                                            inspections, N REFUSED DIALS
//	:213  mapped port: retries: N, port: …      waiting for docker to publish a
//	:216  mapped port: check target: retries: N mapping — NO dials at all, and
//	                                            :213 exits via <-ctx.Done(),
//	                                            skipping the last inspection
//	:191  detect internal port: retries: N      i is never incremented in that
//	:194  detect internal port: check target:   loop, so N is always 0
//
// The report built from this number says "N dials this box's published address
// REFUSED". On a `mapped port:` cause that sentence would name dials that never
// happened, and on a `detect internal port:` one it would print a confident zero.
// ` address:` appears in exactly one of the three, which is what makes it the
// anchor: the other two put `, last err:` or `, port:` where this expects a space.
var waitRetryCount = regexp.MustCompile(`check target: retries: (\d+) address:`)

// One envelope, nine inner shapes, and they do not share a verdict.
//
// checkTarget inspects the container before every dial, and everything it can
// come back with is wrapped in the SAME "check target: retries: N address: …"
// string (wait/host_port.go:262-263) — so the envelope says which loop failed and
// nothing whatever about why. checkTarget and checkState (wait/wait.go:37-57)
// enumerate what can be inside it:
//
//	get state: …: context deadline exceeded   the budget ran out mid-call
//	get state: <other docker error>           the call FAILED: the daemon
//	get state: …: No such container: <id>     the call was ANSWERED: it is gone
//	container crashed with out-of-memory      the box's memory, not the image
//	container exited with code 137 | 143      something KILLED it: the box
//	container exited with code <other>        the code cannot decide it
//	unexpected container status "removing"    something is REMOVING it: the box
//	unexpected container status "dead"        a remove that failed after the kill
//	unexpected container status "created"     the image never started
//
// That last row is the WHOLE residue, and the two obvious other candidates are
// not missing from it — they cannot arrive. checkState answers `state.Running`
// before anything else (wait/wait.go:48), and StateString returns "paused" and
// "restarting" only from inside `if s.Running` (moby container/state.go), with
// SetRestarting setting Running itself — so a paused or restarting container
// returns nil from this check and never reaches the default at all. "dead" is
// no residue either: both things that produce it are removals. cleanupContainer
// sets Dead once the stop has succeeded and before it releases the layer and
// the container root, so a force-remove that fails in either of those leaves it
// (moby daemon/delete.go), and a daemon restarting after a crash mid-removal
// sets the same flag deliberately (moby daemon/daemon.go, the RemovalInProgress
// branch of restore). Same actor as "removing", same predicate.
//
// Those splits are one string apart and carry opposite verdicts. docker prints
// state.ExitCode verbatim, so a container the OOM killer took out and an image
// that exits 1 every run differ by three characters. A 404 is an ANSWERED call,
// which is why it cannot be read as a daemon falling behind: load does not
// delete containers. And "removing" is not a variant of "exited" — it is what a
// force-remove looks like while it runs, because containerRm sets
// RemovalInProgress BEFORE it kills (moby daemon/delete.go) and StateString
// tests that flag ahead of the exited case (moby container/state.go).
//
// The predicates below name them one at a time. An earlier version of this file
// excluded ONE of them from the budget branch and let the other two through,
// which is the shape a blocklist always fails in: the branch then asserted "the
// daemon answered every one of those inspections" over a cause printed three
// lines above saying the last one did not.

// exitCode — the code checkState printed, or -1. checkState formats it verbatim
// from state.ExitCode (wait/wait.go:53), so this reads back exactly what docker
// reported and not an interpretation of it.
//
// -1 is ambiguous on purpose-free grounds: docker can report a real -1, and the
// regexp accepts it. That is safe only while every caller tests membership of an
// explicit set. A predicate written as `exitCode(cause) >= 0` would read "no code
// here" as a code, so a caller that needs presence must gain a second return
// value rather than compare against the sentinel.
func exitCode(cause error) int {
	if cause == nil {
		return -1
	}
	m := containerExitCode.FindStringSubmatch(cause.Error())
	if m == nil {
		return -1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return n
}

var containerExitCode = regexp.MustCompile(`container exited with code (-?\d+)`)

// killedBySignal — the container died on a signal rather than returning: docker
// reports 128+N for a process killed by signal N, so 137 is SIGKILL and 143 is
// SIGTERM. Those two are what docker's own kill and stop paths produce, and no
// service image chooses to die on them. That is the whole of the claim.
//
// The range stops there deliberately. containerd synthesises 128+N into the SAME
// integer field a program's own exit(2) writes, so the code cannot prove which
// happened; {137,143} is defensible only because nothing exits that way on
// purpose. Widening to all of 128+N would sweep in 130 — a code any program can
// reach by returning it, and equally what a process that died on SIGINT reports;
// and SIGINT is a signal one of these stands is really sent, since
// postgres:16-alpine declares STOPSIGNAL SIGINT while redis:7-alpine and
// hashicorp/vault:1.15 declare none and take the SIGTERM default. What an image
// reports AFTER handling the signal it was sent is the image's own business —
// docker records the signal it sends AND the integer the process ended with, and
// never which of the two caused the other; exitCode() a few lines up reads that
// second number back, which is exactly why it cannot be asked the first question
// — so the claim here stops at the one thing that holds: 130 has an innocent
// reading and {137,143} have none. Every code outside {137,143} therefore falls
// to containerGone, which says the code cannot decide it rather than deciding it
// wrongly.
func killedBySignal(cause error) bool {
	switch exitCode(cause) {
	case 137, 143:
		return true
	default:
		return false
	}
}

var containerStatusRe = regexp.MustCompile(`unexpected container status "([^"]*)"`)

// containerStatus — the status checkState printed, or "". checkState quotes
// state.Status verbatim (wait/wait.go:55) for anything that is neither running
// nor exited, so this reads back what docker reported.
func containerStatus(cause error) string {
	if cause == nil {
		return ""
	}
	m := containerStatusRe.FindStringSubmatch(cause.Error())
	if m == nil {
		return ""
	}
	return m[1]
}

// beingRemoved — the inspection caught the container mid-removal. This is not a
// variant of "it exited": moby's containerRm sets RemovalInProgress BEFORE it
// kills (daemon/delete.go), and StateString tests that flag ahead of the exited
// case (container/state.go), so a force-remove is visible as "removing" for the
// whole window between the SIGKILL and the 404 — a window strictly WIDER than
// the 137 one, and therefore the likelier of the two to be sampled. "dead" rides
// along: the api documents it as a container that failed to be deleted, which is
// the same actor and the same verdict.
//
// A stray reaper is why this branch exists. It reaches this function as three
// different strings depending on which 100ms poll lands — 137, "removing", then
// "No such container" — and before this branch existed the middle and widest of
// the three was the one answered with a promise that the failure was permanent.
func beingRemoved(cause error) bool {
	switch containerStatus(cause) {
	case "removing", "dead":
		return true
	default:
		return false
	}
}

// containerGone — an inspection found the container no longer running, and
// nothing in the cause says who ended it. This branch used to promise the
// failure would still be here next run; it cannot. docker collapses "returned
// 0" and "was stopped and handled it" into the same integer, so exit 0 from a
// service image that has no successful terminal state to reach is a stop, not a
// finding — and it landed here wearing the same string as a genuine one.
func containerGone(cause error) bool {
	if cause == nil {
		return false
	}
	return strings.Contains(cause.Error(), "container exited with code") ||
		strings.Contains(cause.Error(), "unexpected container status")
}

// outOfMemory — the kernel killed the container (wait/wait.go:51, tested before
// the exit status, so it never reaches containerGone). This is the box running
// out of memory, which is the machine and does go away on a rerun.
func outOfMemory(cause error) bool {
	return cause != nil && strings.Contains(cause.Error(), "OOMKilled")
}

// waitRanOutOfTime — the wait ended on its own budget rather than on a fact about
// the container. The deadline surfaces THROUGH a docker call, so this string is
// what an inspection that never came back looks like too — which is why every
// branch that reads it also weighs the count against the budget.
func waitRanOutOfTime(cause error) bool {
	return cause != nil && strings.Contains(cause.Error(), "context deadline exceeded")
}

// inPortWait — the cause carries externalCheck's own report, so its count means
// answered inspections and refused dials rather than anything else.
func inPortWait(cause error) bool { return waitRetries(cause) >= 0 }

// containerVanished — an inspection was answered with a 404. ContainerInspect
// returns cli.get's error unchanged in moby/client v0.4.0, and every daemon
// error is wrapped as "Error response from daemon: %w" (request.go:306) around
// the daemon's own body, "No such container: <id>" (moby container/view.go). So
// this arrives as "get state: Error response from daemon: No such container:
// <id>", and the substring below is the part of it that is stable.
//
// It is the one shape in this envelope where the docker call SUCCEEDED and still
// produced no state, and that is why it cannot be left to the branch below it:
// that branch reads missing state as a daemon falling behind and sends the reader
// to the load line at the top of the report. Load does not remove a container
// mid-wait. Something outside the run did, and the two want different checks.
func containerVanished(cause error) bool {
	return inPortWait(cause) && strings.Contains(cause.Error(), "No such container")
}

// daemonStoppedAnswering — the inspection that ended the wait came back with no
// container state at all: not a deadline, and not a fact about the container.
// The two container facts are excluded by the switch, which checks them first —
// and the exited-container known-bad is what holds that order in place.
func daemonStoppedAnswering(cause error) bool {
	return inPortWait(cause) && !waitRanOutOfTime(cause)
}

// waitOutlivedItsBudget — the wait used up its whole budget SLEEPING, which is
// the only shape from which "the daemon was not the thing that was stuck" follows.
//
// The count alone does not carry that. It is a numerator: each round is one
// answered inspection, one refused dial and one 100ms sleep, so N rounds account
// for N×100ms of a window whose length is known — the stand's budget, because the
// deadline fired. When the sleeps are most of that window, everything else fit in
// the rest and the daemon was answering at that rate. When they are a sliver, the
// window went into the docker calls, and the same count that looks like proof of
// a healthy daemon is proof of the opposite; that is the branch below this one.
//
// Half is the bar, and it is deliberately conservative in the safe direction: the
// budget covers the whole ForAll, so the port wait's own share is at most that
// and usually less (postgres spends ~20s in its log wait first). Measuring the
// sleeps against the LARGER number can only make this predicate refuse to claim
// a healthy daemon that was one — never the reverse.
//
// The deadline term and this branch's POSITION are jointly redundant, and that is
// worth stating because it makes each of them look unnecessary on its own. The
// branch above requires the absence of a deadline and this one requires its
// presence, so the two are disjoint: delete either the term or the ordering and
// nothing changes — a mutation of one alone cannot be caught, and a reader who
// tries either will conclude it was dead weight. Delete BOTH and an inspection
// that failed outright is reported as a wait that slept through its budget, which
// is the misattribution this whole file exists to stop.
func waitOutlivedItsBudget(cause error) bool {
	n := waitRetries(cause)
	return n >= 0 && waitRanOutOfTime(cause) &&
		time.Duration(n)*waitPollInterval >= standReadyTimeout/2
}

// waitPollInterval — testcontainers' poll interval for a port wait, from
// NewHostPortStrategy. Stated here rather than read from the library because
// nothing exports it; it is used only to turn the count into a span, so a
// version that changes it makes that span wrong while the count stays true.
const waitPollInterval = 100 * time.Millisecond

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
