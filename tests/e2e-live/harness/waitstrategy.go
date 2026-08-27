// Readiness of the stand's own dependencies (PG / Redis / Vault).
//
// Deliberately NOT behind the `e2e_live` tag: the guard in waitstrategy_test.go
// has to run docker-free, in `make check` and in the gate's own unit-guard step,
// because the regression it catches is one nobody sees until a gate run goes red
// under load — which is the moment the answer is needed and the slowest moment
// to get it.
//
// Every strategy here waits for the property the harness is about to USE, and
// each one ends at the mapped host port, because that is the hop that fails.
// testcontainers prints `🔔 Container is ready` off an in-container signal; the
// harness then dials 127.0.0.1:<mapped> from the host, and under load the port
// binding is not always serving by then. Three of the four NIM-406 gate flakes
// were exactly that gap — one per container, and each container had a different
// hole in it:
//
//   - postgres — startPostgres passed testcontainers.WithWaitStrategy(ForLog×2),
//     and that was the container's ONLY wait. There is no module default
//     underneath it to fall back on: modules/postgres never sets WaitingFor at
//     all (postgres.go appends WithEnv / WithExposedPorts / WithCmd and nothing
//     else), and the port check lives in BasicWaitStrategies() — an OPT-IN
//     customizer this harness never called, which is where upstream's "Without
//     this, the tests will be flaky on those OSes!" actually sits. So the check
//     was never added rather than deleted. Same hole, and worth stating
//     correctly: reaching for the module default would not have closed it. Seen
//     as `keeper init: pg ping: dial tcp 127.0.0.1:34492: connect: connection refused`.
//   - vault — waited on the log line `Root Token:` and nothing else, so the
//     harness's next act (an HTTP call to sys/mounts/pki) was the first thing to
//     touch the port. Seen as `InitVaultTestSecrets: enable pki mount: …
//     dial tcp 127.0.0.1:33242: connect: connection refused`.
//   - redis — the module default DOES check the port, on a 10-second budget that
//     also has to cover the docker-daemon round-trips the check itself makes
//     (externalCheck calls checkTarget on every attempt). Seen as `wait until
//     ready: external check: … get state: … context deadline exceeded`.
//
// Note which way these move. The waits get STRICTER, never looser. A readiness
// wait loosened to make a suite quieter trades a loud infra failure for a silent
// one — the trade docs/testing/README.md argues against and the one NIM-349
// refused for these same stands. The only number that grows here is the budget,
// and a budget is not a property: it decides how long an honest bring-up may
// take, not what "up" means.
package harness

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go/wait"
)

// Container-side ports of the stand dependencies. Named because the wait
// strategies and the guard have to agree on them, and a typo in either would
// silently produce a strategy that waits for a port nothing listens on.
const (
	postgresContainerPort = "5432/tcp"
	redisContainerPort    = "6379/tcp"
	vaultContainerPort    = "8200/tcp"
)

// standReadyTimeout — how long one stand dependency gets to become usable.
//
// One named number instead of three anonymous ones. Before NIM-406 postgres
// waited 60 s (chosen here), vault 45 s (chosen here) and redis 10 s (a library
// default nobody in this repo ever chose), and the shortest of the three was the
// one that expired. Sizing them apart implied the three containers had different
// bring-up costs; they do not — they share one docker daemon, and it is the
// daemon under contention that sets the cost.
//
// Why 2 minutes: NIM-349 measured a vault container blowing a 45 s budget at
// loadavg ~12, and this gate is run at 12-25 with `-p 1` on a box shared with
// other sessions. 2 min is ~2.7× the point where the old budget was observed to
// break, and 3-12× anything these three have been seen to need when healthy.
//
// It is deliberately BOTH the per-check timeout and the deadline on the whole
// set. wait.MultiStrategy runs its checks one after another (wait/all.go), so
// without the shared deadline two checks at 2 min each would bound ONE container
// at 4 min rather than 2. What is worth budgeting is "this container became
// usable", not "each probe got its turn".
//
// This is the one dial here that moves probability rather than meaning, so it is
// the one to be suspicious of: if a container needs more than two minutes to
// serve its port, that is a finding about the machine, and the honest outcome is
// the check failing and saying so.
const standReadyTimeout = 2 * time.Minute

// standCount — how many dependency containers NewStack raises, back to back.
// Named so the bound below is arithmetic instead of a number someone picked
// once; waitstrategy_test.go holds it to the table it guards, so a fourth stand
// cannot be added without the budget moving with it.
const standCount = 3

// standBringUpTimeout — the ctx covering all of them together.
//
// Derived, never written as its own number, because writing it as its own number
// is the bug. NIM-406 is a budget silently capped by an outer one nobody had
// compared it against, and the first cut of this very fix reproduced that one
// level up: three containers at 2 min each come to 6, while NewStack had been
// handing them a flat 5 min ctx since long before. Under the budgets NIM-406
// replaced (60 + 10 + 45 s) the sum fit with room to spare, so the cap had never
// been load-bearing and nothing pointed at it.
//
// What that would have cost is worth spelling out, because it lands exactly on
// the ticket's own symptom. The stands come up in sequence, so the ctx is spent
// in order: had postgres and redis taken 100 s each, vault — third in line, and
// the container these gate failures kept dying on — would have got what was
// left rather than its own 2 min, and failed as `context deadline exceeded`
// from the parent instead of naming itself. The declaration in setupdecl.go
// still labels that STAND-SETUP, so the gate would have stayed honest about
// WHOSE failure it was while quietly lying about WHICH.
//
// The extra minute covers the work between the waits that shares this ctx and is
// not a wait strategy at all: image bookkeeping, ConnectionString, the pgx pool.
const standBringUpTimeout = standCount*standReadyTimeout + time.Minute

// postgresWaitStrategy waits for the log line AND for docker to serve the port.
//
// Both halves are load-bearing and neither implies the other: the log line is
// printed twice because PG boots, shuts down and reboots during initdb (the
// first "ready" is not the server the harness gets), while the port check is the
// only one of the two that involves the host at all.
func postgresWaitStrategy() wait.Strategy {
	return wait.ForAll(
		wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(standReadyTimeout),
		wait.ForListeningPort(postgresContainerPort).
			WithStartupTimeout(standReadyTimeout),
	).WithDeadline(standReadyTimeout)
}

// redisWaitStrategy is the module's own pair of checks on the shared budget.
// The property was never wrong for redis — only the 10 s it was given.
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
// and binds its listener before the dev-mode server has finished unsealing, and
// the harness's first call is not a dial but sys/mounts/pki — a request that
// needs an unsealed, active Vault. /v1/sys/health returns 200 only for
// initialized + unsealed + active, which is that condition exactly, so this
// waits for the thing InitVaultTestSecrets is about to need rather than for a
// line that precedes it.
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

// soulReadyTimeout — how long one soul container gets to finish booting systemd.
//
// Its own number rather than standReadyTimeout: this is a plain Debian image
// reaching a target, not a database restoring itself, and it is not one of the
// three stands standBringUpTimeout is derived from. Sixty seconds is ~120× the
// 300-500 ms a healthy boot has been measured at here, so a container that blows
// it is a finding about the machine rather than a budget to raise.
const soulReadyTimeout = 60 * time.Second

// systemdFinishedBooting reports whether `systemctl is-system-running --wait`
// said the boot is over.
//
// The exit code cannot answer that, and the mistake is not a near miss: it
// returns 1 for `degraded` — normal in a slim image with half the units removed,
// and genuinely ready — and 1 AGAIN when it could not reach the bus at all,
// which is systemd not yet being up. Accepting 1 therefore accepts a container
// that has not started booting, and `--wait` does not save it: waiting is
// something it does after connecting, so a failure to connect returns instantly.
// The two are told apart by what the command printed and by nothing else, which
// is the same shape as reading `N added` out of update-ca-certificates instead of
// trusting its exit code (container.go, step 4b).
//
// What it cost, in this repo, on the run this was found on: the readiness probe
// answered `Failed to connect to bus: No such file or directory` in 164 ms, the
// container was declared ready, and update-ca-certificates then ran against a
// half-booted system. It creates two temp files in /tmp and reads them ~80 lines
// later; in between, systemd-tmpfiles-setup.service reached the `D /tmp 1777 root
// root -` line of /usr/lib/tmpfiles.d/tmp.conf, and `D` with --remove empties the
// directory. The files vanished under the running script, dash failed the
// redirect on line 168 and exited 2, and the gate went red on
// TestL3bModuleDeliveryLive_SynthesisFetchHotRegister — a test whose subject has
// nothing to do with certificates. Reproduced outside the suite at 2 runs in 12,
// and the correlation with the probe was total: every failure had answered
// `Failed to connect to bus` in 100-164 ms, every pass had answered `running` in
// 300-485 ms.
//
// Matched per line, not against the whole body: the reader carries stdout and
// stderr multiplexed together with no ordering guarantee between them, so a
// single stray warning on either would make an exact whole-body comparison
// reject a system that is genuinely up — and that failure mode is a silent
// 60-second stall per container. A line equal to one of these two tokens is
// something only systemctl writes; the seven other states it can print, and the
// bus error, contain no such line.
func systemdFinishedBooting(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		switch strings.TrimSpace(line) {
		case "running", "degraded":
			return true
		}
	}
	return false
}

// soulWaitStrategy waits for systemd inside a soul container to finish booting.
//
// Here rather than inline in container.go so that the guard can reach it: this
// file is untagged and container.go is not, so a strategy declared there is
// unreachable from the docker-free tests — which is where a readiness check has
// to be checkable, per this file's opening note.
//
// The exit-code matcher stays deliberately wide. Tightening it to 0 would reject
// `degraded`, and degraded is the expected steady state of this image: the
// Dockerfile deletes most of the unit symlinks, so units that remain enabled and
// have nothing to do are a normal outcome. The narrow check is the response
// matcher; the exit code is only there to not throw away a ready container.
func soulWaitStrategy() wait.Strategy {
	return wait.ForExec([]string{"systemctl", "is-system-running", "--wait"}).
		WithExitCodeMatcher(func(code int) bool { return code == 0 || code == 1 }).
		WithResponseMatcher(func(body io.Reader) bool {
			out, err := io.ReadAll(body)
			if err != nil {
				return false
			}
			return systemdFinishedBooting(string(out))
		}).
		WithStartupTimeout(soulReadyTimeout)
}
