//go:build e2e

// Readiness of the stand's own dependencies (PG / Redis / Vault) for L3a.
//
// This is NIM-406's waitstrategy.go (tests/e2e-live/harness/) one tier down, and
// deliberately a sibling rather than a variation: same properties, same shared
// budget, same guard. What NIM-406 established for e2e-live was never a fact
// about that tier — it is a fact about waiting on a container through a mapped
// port, and L3a does that three times per test, forty times per run.
//
// The reason it needed doing here too is that L3a was still carrying the
// PRE-NIM-406 shape, unchanged, on every one of those forty stands:
//
//   - postgres — testcontainers.WithWaitStrategy(ForLog×2) and nothing else. No
//     host-port check anywhere under it: modules/postgres never sets WaitingFor
//     itself, and the port check lives in BasicWaitStrategies(), an opt-in
//     customizer this harness never called.
//   - redis — no strategy passed at all, so the module default's 10 s applied. A
//     budget nobody in this repo chose, and the shortest of the three, so it is
//     the one that expires first.
//   - vault — wait.ForLog("Root Token:"), a line vault prints before dev-mode
//     has finished unsealing, while the harness's next act is an HTTP call.
//
// That is not a hypothesis about what could go wrong here. NIM-469's own run 1
// died as
//
//	keeper init: pg ping: failed to connect to `user=keeper database=keeper`:
//	127.0.0.1:32840: dial error: dial tcp 127.0.0.1:32840: connect: connection refused
//
// which is the postgres bullet above, and character-for-character the failure
// NIM-406 recorded for the same container in the other tier. The old L3a
// classifier called it INFRA/STACK_BRINGUP — "the stack never came up, nothing
// to conclude about the code" — on a signature list that matched the words
// `keeper init failed`. It was a finding about this file, and the tool built to
// triage L3a was the thing saying not to look.
//
// It also explains the shape of the whole ticket. These waits are proxies that
// hold on an idle machine and stop holding under contention, so each test passes
// alone and a full serial run of forty drops two or three — a different two or
// three each time, since which container loses the race is a matter of what else
// the daemon was doing. "Green alone, red in company" is what a load-sensitive
// proxy looks like from the outside.
//
// Note which way these move. The waits get STRICTER, never looser. A readiness
// wait relaxed to quieten a suite trades a loud infra failure for a silent one.
// The only number that grows is the budget, and a budget is not a property: it
// decides how long an honest bring-up may take, not what "up" means.
package harness

import (
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go/wait"
)

// Container-side ports of the stand dependencies. Named because the strategies
// and the guard have to agree on them, and a typo in either would quietly
// produce a strategy waiting on a port nothing listens on.
const (
	postgresContainerPort = "5432/tcp"
	redisContainerPort    = "6379/tcp"
	vaultContainerPort    = "8200/tcp"
)

// standReadyTimeout — how long one stand dependency gets to become usable.
//
// One named number instead of three anonymous ones. Before this, L3a gave
// postgres 60 s (chosen here), vault 45 s (chosen here) and redis 10 s (a
// library default nobody in this repo chose) — and the shortest was the one that
// expired. Sizing them apart implied the three had different bring-up costs;
// they do not. They share one docker daemon, and it is the daemon under
// contention that sets the cost.
//
// The value matches e2e-live's deliberately. NIM-349 measured a vault container
// blowing a 45 s budget at loadavg ~12, and L3a runs 40 stands back to back on
// the same box, frequently beside another session's suite — if anything it sits
// further up that curve, not lower.
//
// Worth stating plainly, because it looks like a cost: this does NOT slow a
// healthy run. A wait strategy returns when its property holds, and on a warm
// daemon these hold in a couple of seconds. The budget only changes what happens
// on a bad day — a container gets two minutes to become usable instead of
// failing early with a misleading label. `make e2e` allows the suite 30 min
// against a measured ~8.5, so the headroom is there; and if a stand really needs
// two minutes to serve its port, that is a finding about the machine, and the
// honest outcome is the check failing and saying which container it was.
const standReadyTimeout = 2 * time.Minute

// standCount — how many dependency containers NewStack raises, back to back.
// Named so the bound below is arithmetic rather than a number someone picked
// once; waitstrategy_test.go holds it to the table it guards, so a fourth stand
// cannot be added without the budget moving with it.
const standCount = 3

// standBringUpTimeout — the ctx covering all of them together.
//
// Derived, never written as its own number, because writing it as its own number
// is the bug. NewStack had been handing the three containers a flat 5 min ctx,
// under budgets (60 + 10 + 45 s) that summed to well under it — so the cap had
// never been load-bearing and nothing pointed at it. Raise the parts to 2 min
// each without touching the whole and the outer bound silently becomes the real
// one.
//
// What that costs lands exactly on this ticket's symptom. The stands come up in
// sequence, so the ctx is spent in order: had postgres and redis taken 100 s
// each, vault would have got the remainder rather than its own budget and failed
// as `context deadline exceeded` from the parent — without naming itself. The
// declaration in setupdecl.go still labels that STAND-SETUP, so the run would
// have stayed honest about WHOSE failure it was while lying about WHICH.
//
// The extra minute covers the work between the waits that shares this ctx and is
// not a wait strategy at all: image bookkeeping, ConnectionString, the TLS
// material, writing keeper.yml.
//
// standBringUpAttempts is in the product for the same reason standCount is. A
// stand may be started twice when the daemon — not the container — was what
// failed (daemonhealth.go), and a bound that did not know that would let the
// first stand's retry eat the third stand's budget, which is the precise defect
// this constant exists to prevent. It is a cap, not a cost: nothing waits longer
// than the property takes, and the retry is not spent unless the daemon probe
// says the machine is the reason.
//
// Worth stating because it is the one uncomfortable number here: at the cap, one
// test can consume 13 of the suite's 30 minutes, and the tests after it are
// reported NOT-RUN. That is the correct outcome and not a regression in it — a
// box where three stands each need two full minutes twice is a box whose run
// certifies nothing, and the honest result is one loud failure that names the
// machine plus an explicit NOT-RUN for the rest, rather than forty tests sharing
// a starved budget and failing in a scatter nobody can attribute.
const standBringUpTimeout = standCount*standBringUpAttempts*standReadyTimeout + time.Minute

// postgresWaitStrategy waits for the log line AND for docker to serve the port.
//
// Both halves are load-bearing and neither implies the other. The line is
// printed twice because PG boots, shuts down and reboots during initdb — the
// first "ready" is not the server the harness gets, which is what the occurrence
// count is for — while the port check is the only one of the two that involves
// the host at all. `keeper init` dials 127.0.0.1:<mapped>, so that is the hop to
// wait on.
func postgresWaitStrategy() wait.Strategy {
	return wait.ForAll(
		wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(standReadyTimeout),
		wait.ForListeningPort(postgresContainerPort).
			WithStartupTimeout(standReadyTimeout),
	).WithDeadline(standReadyTimeout)
}

// redisWaitStrategy — the module's own pair of checks, on a budget this repo
// chose. The property was never wrong for redis; L3a simply never stated one, so
// it inherited 10 s.
func redisWaitStrategy() wait.Strategy {
	return wait.ForAll(
		wait.ForListeningPort(redisContainerPort).
			WithStartupTimeout(standReadyTimeout),
		wait.ForLog("* Ready to accept connections").
			WithStartupTimeout(standReadyTimeout),
	).WithDeadline(standReadyTimeout)
}

// vaultWaitStrategy waits for the port and then for the API to answer healthy.
//
// The port check alone would still be a proxy here. Vault prints `Root Token:`
// and binds its listener before dev-mode has finished unsealing, and the
// harness's first call is not a dial but a KV write of the JWT signing key — a
// request that needs an unsealed, active Vault. /v1/sys/health returns 200 only
// for initialized + unsealed + active, which is that condition exactly, so this
// waits for the thing InitVaultTestSecrets is about to need rather than for a
// line that merely precedes it.
func vaultWaitStrategy() wait.Strategy {
	return wait.ForAll(
		wait.ForListeningPort(vaultContainerPort).
			WithStartupTimeout(standReadyTimeout),
		wait.ForHTTP("/v1/sys/health").
			WithPort(vaultContainerPort).
			WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
			WithStartupTimeout(standReadyTimeout),
	).WithDeadline(standReadyTimeout)
}
