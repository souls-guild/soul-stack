# Soul Stack Testing

Index of test levels. Each level is a separate mechanism; they don't compete
and are added to each other. Source of truth by design -
[ADR-023](../adr/0023-trial-test-runner.md) (Trial)
and [ADR-039](../adr/0039-e2e-testing.md) (E2E).

## Levels

| Level | Where | Build-tag | What covers | Run |
|---|---|---|---|---|
| **L0** - unit | `<module>/<pkg>/*_test.go` | no | Pure function logic. No network/DB/containers. | each PR, `make test` |
| **L1** - integration | `<module>/<pkg>/integration_test.go` | `integration` | testcontainers per-package (PG / Redis / Vault), real CRUD calls. | each PR, `make test-integration` (needs docker) |
| **L2** - Trial | `examples/destiny/<name>/_trial/`, `examples/service/<name>/scenario/<n>/tests/` | no | Hermetic prerender + migration-assert on fixtures (`soul-trial`). Bind `--modules <alias>=<path>` or a plugin step's `params:` are checked by nobody and the case says so ([soul-lint.md](../soul-lint.md#soul-trial-takes-the-same-flag)). | each PR via `make build` + `soul-trial` |
| **L3a** - E2E fast-loop | `tests/e2e/` | `e2e` | testcontainers (PG/Redis/Vault) + Keeper-process in-process + soul-stub. Contract tests apply_runs lifecycle / RBAC / audit / MCP. | every PR (when L3a-imp slice stabilizes), `make e2e` |
| **L3b** - E2E smoke | `tests/e2e-live/` | `e2e_live` | Real `soul`-binary in a privileged Debian-12 container (systemd-PID-1) + Keeper process + mTLS + real apply. Flagship scripts. | **a tag** (`make e2e-live-gate` is a blocking job in `release.yml`, NIM-879) + on demand; **feature-complete** (5 slices L3b-1..L3b-5 = done): real CSR Bootstrap + `smoke-nginx-live` (apt + systemd) + module delivery + the plugin channel. ★ The broad `make e2e-live` is dispatched from `nightly.yml`, which has **no schedule and had never run once** before NIM-879 — the line here used to say it "really drives nightly", and that is exactly the false green the ticket was about. ★ The multi-host `redis-cluster-live` fixture and every service-lifecycle case left with `examples/service/redis` (NIM-871), taking the blocking gate from nine tests to three. L3b-6 (drift-live) - done: `TestL3bDriftLive_HelloWorld` runs drift-check on a live soul through a real `core.file.Plan` (module `core.file.present`), and not stub-Plan like L3a |
| **L3c** - E2E k8s | `tests/e2e-k8s/` | `e2e_k8s` | kind-cluster, real K8s-deployment Keeper + Soul + Redis-Cluster + PG. HA cases (Watchman, Toll, leader-failover). | weekly / pre-release, **L3c-1..L3c-5 part A ready** (single-keeper ping, multi-keeper + Soul CSR Bootstrap, kill-leader failover, Toll degraded-mode); L3c-5 part B (redis-cluster resharding) - framework + t.Skip to in-cluster git-server-pod |
| **L4** – manual soak + cloud live-run | — | — | Manual pre-release testing under load **+ repeatable cloud orchestrator** `scripts/e2e-cloud/` (create / day-2 / destroy via keeper's Operator API on VM, teleport), see [e2e-cloud.md](e2e-cloud.md). | first product post / on-demand |

## Where to write tests

- **Pure function/classifier/parser** → L0 in `<module>/<pkg>/<file>_test.go`.
- **CRUD layer / DB migration / Vault client** → L1 `integration_test.go` with `//go:build integration`.
- **Render pipeline destiny / scenario / state migration** → L2 Trial fixture (`_trial/`).
- **Scenario-runner contract → apply_runs → audit → metrics** → L3a in `tests/e2e/`.
- **Real host mutation (filesystem/systemd)** → L3b in `tests/e2e-live/`.
- **HA script (multi-Keeper / failover)** → L3c in `tests/e2e-k8s/` (when it works).
- **Live cloud day-2 / create-destroy on persistent keeper** → `scripts/e2e-cloud/`
(not Go-harness, bash over Operator API via teleport; see [e2e-cloud.md](e2e-cloud.md)).

## Communication with CI

- `make check` drives L0 + L2 (via `lint`). L1 / L3a / L3b / L3c - separate
targets on request (require docker/kind).
- **`make check-all` = `check` + L1 + L3a — the one command whose green result
means what a green CI run means.** `check` is docker-free by design and
therefore says nothing about L1 or L3a; those were two different claims that
both ended in the word "passed", and neither implied the other. That is how a
rotted L1 suite (NIM-221) and four L3a tests failing since ADR-029 (NIM-317)
stayed invisible for a whole release. `check` now prints what it did NOT run;
`check-all` runs it. L3b stays outside both, and outside CI's push jobs — but **not**
outside CI: since NIM-879 `release.yml` runs `make e2e-live-gate` on a tag and goreleaser
is behind it. That covers the tag pipeline and nothing else: a release created by hand
still publishes, and `apt-publish.yml` still mirrors it (RELEASING.md step (e) names the
three residual paths).
One difference from CI survives on purpose: CI gives each tier its own runner,
`check-all` gives them one docker daemon and starts L3a right after ~300
container-starting L1 packages. A failure at container startup there is that
contention — rerun the package alone before believing it, and do not loosen a
readiness wait to make it go away.
- **A red tier no longer silences the tiers behind it** (NIM-373). `check` used
to be a make prerequisite chain: the first failure stopped it, and the tiers that
never ran left no trace whatsoever, so a gate that stopped after five of twenty
was still reported as "the gate was run". Both gates now drive their tiers
through [scripts/gate.sh](../../scripts/gate.sh), which runs each one, keeps
going past a failure, and ends in a table with three outcomes rather than two:
PASS, FAIL and **NOT RUN** — the last naming the tier that suppressed it. The
exit code is non-zero unless every tier is PASS. This is the other half of what
`check` already did for the tiers it never starts (NIM-316): it said what it
skipped, and now it also says what it never reached.
The one dependency kept is causal. `tier@build` in `GATE_CHECK_TIERS` means "if
compilation failed, running this says nothing the build failure has not already
said", and those tiers report NOT RUN. A bundle drift or a broken doc link
suppresses nothing, because neither predicts anything about the integration
suite. The tier order is unchanged on purpose: putting the static checks in
front of the tests would only move which tier does the silencing, and with the
chain gone the order decides nothing at all.
Where the loss actually was differs by gate, and both are worth knowing.
Locally, `check-webui` sits in the middle of `check`, so a drifted bundle kept
`test-integration` and `e2e` from running at all — the expensive half of the gate
lost to the cheapest tier in it. In CI those two are separate jobs and were never
at risk; what was at risk there is the nine deterministic tiers sitting behind
`test` inside the `check` job, OpenAPI and bundle drift among them. One fix, two
very different prices.
- `make check` still **compiles** the levels it cannot run: `vet-tags` vets
L1/L3a/L3b/L3c sources under their own build tags without starting anything
(docker-free). Without it a suite goes unbuildable unnoticed between docker runs -
NIM-207 found L1 for `soul/cmd/soul` broken for several tickets by a signature
change that nothing rebuilt.
- Before a batch commit of a **major** feature, a local live gate is required
`make e2e-live-gate` (curated L3b subset, docker; see
Local live gate for major features).
`make check` does not include it.
- GitHub Actions workflow for regular run L3a/L3b/L3c - separate task
([ADR-039 § 7](../adr/0039-e2e-testing.md)),
is not added to the current slice.

### Pre-release checklist (docker dependent outside `make check`)

`make check` is deliberately docker-free, so anything that requires containers
drops out of it and is NOT caught on every PR. Before release these targets
**be sure to run it manually** - otherwise stale-fails accumulate unnoticed.
The last inventory (NIM-221) found 15 of them at once in L1: tests still seeding
scope selectors NIM-128 had removed, and a helper that kept binding coven names
as incarnation memberships after NIM-124 changed the roster axis. Both are the
same failure mode - a deliberate contract change that nothing re-ran the suite
against.

**Measured, so that the next coordination does not have to rediscover it.** On
2026-07-29 the R5 branch was pushed to the remote and CI ran over a release
branch for the first time. `make check` was green at that moment, and had been
green for the whole release. The first CI run found **nine failing tests** it had
never mentioned: two in L1 (a fanout collector that mistook a scheduler stall for
event loss, and a self-lockout invariant shadowed by a caller-rights gate) and
seven in L3a (three redis tests drifted from their own example's covenant, four
examples unable to create an incarnation since ADR-029 made the service registry
a precondition - they had been red for a whole release). None of the nine was
subtle; all nine were simply outside what `make check` runs.

So "green locally" was never evidence that the release was checked, and it will
read that way again unless someone says otherwise out loud. That is what
`make check-all` is for, and why `make check` now prints the tiers it skipped.
Measure release readiness with `check-all`, never with `check` alone - and read a
CI result **per sha**, not per branch. `make check-ci` asks that question for a
sha it derives from git ([scripts/ci-status.sh](../../scripts/ci-status.sh)):
it separates "verified" from "failed" from "no verdict", where no-verdict covers
an unpushed commit, a run still going, and a run **evicted** by the next push -
reported `cancelled`, which sits next to `failure` in the run list and next to
`success` in memory. Eviction no longer happens on `main` or `release/*`
(concurrency exception in `.github/workflows/ci.yml`, NIM-339), but a queued run
is still not a verdict.

- **L1 — `make test-integration`** (build-tag `integration`, testcontainers
PG / Redis / Vault, docker is needed). Covers keeper-integration, which
docker-free `make check` is missing: scenario-dispatch, state-migrate,
gRPC / EventStream, recovery, topology, Redis coordination,
Voyage orchestration. **The most important item on the checklist.**
- **L3a - `make e2e`** (build-tag `e2e`, testcontainers, docker needed) -
contract apply_runs lifecycle / RBAC / audit / MCP when L3a-slice is stable.
- **L3b — `make e2e-live`** (build-tag `e2e_live`, privileged docker) —
smoke on a real `soul` binary; usually nightly, but run it before release.

One-time flake at container start (testcontainers infra — for example a timeout
bringing Vault up) is not a code regression: rerun the affected package in
isolation rather than rolling the change back. Since NIM-569 the bring-up itself
retries first, so a sweep that still reports an INFRA failure has already burned
three containers on it — see [below](#one-red-package-per-sweep-a-different-one-every-time).

**You no longer have to work out which one you got.** When L1 fails, the target
runs [`scripts/classify-l1-failure.py`](../../scripts/classify-l1-failure.py) over
the output and labels every failing package:

- **REGRESSION** — an assertion failed. A finding; rerunning changes nothing.
- **INFRA** — a signature only the container/daemon layer can produce (`wait until
  ready: context deadline exceeded`, `Error response from daemon`,
  `docker-credential-…`). Nothing was asserted, so there is nothing to conclude
  about the code. It prints the `PKG=` line to rerun that package alone.
- **UNCLEAR** — a signature either layer can produce (`connection refused`,
  `CLUSTERDOWN`, `i/o timeout`). Deliberately **not** folded into INFRA: a false
  INFRA label is the failure mode that matters, because that is the one that makes
  a regression disappear. Treat UNCLEAR as a finding until a solitary rerun says
  otherwise.

Why label rather than fix: `CLUSTERDOWN` and a failed assertion arrive in the same
`--- FAIL` shape and have opposite answers, so telling them apart by eye cost a
person an hour per red run — and the cheap way out of that hour ("L1 is flaky,
rerun it") is exactly how a real regression gets waved through. The labelling
downgrades nothing: L1 still fails, the target still exits non-zero, and a failure
that survives a solitary rerun is a finding whatever its label said. In particular
no readiness wait was loosened to make this quieter — that would trade a loud infra
failure for a silent one, which is the trade the `check-all` target argues against.

**Rerun it through the target, not around it:**

```
make test-integration PKG=./internal/<pkg>/
```

A bare `go test -tags=integration ./<pkg>/...` looks equivalent and is not: it
drops `-race`, which `make test-integration` (and therefore CI) passes. Both
print the same word at the end, so "L1 is green" would mean something weaker
from one keyboard than from another, and the weaker meaning is silent about
every data race in the code those suites exercise. A whole-tree sweep that lost
the flag fails on `TestIntegrationSuiteRunsUnderRace` rather than passing
quietly; declaring the weaker run is possible and explicit
(`SOUL_STACK_INTEGRATION_SKIP_RACE=1`).

### One red package per sweep, a different one every time

For months `make test-integration` failed **exactly one** of its ~47 packages per
sweep and a different one each time (NIM-569). That shape is worth recognising
because it rules out the obvious readings by itself: a package that fails on its
own contents fails on every sweep, and 46 of 47 passing rules out "the daemon
cannot keep up".

What it actually was, measured rather than assumed — and note that the first
version of this section got it wrong, which is worth keeping because of *how*:

- the failing sweeps carry this line, quoted here **to the end**:

  ```
  check target: retries: 503 address: localhost:32890: get state:
  Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/<id>/json":
  context deadline exceeded
  ```

- four sweeps on one host: at `INTEGRATION_PARALLEL=4` they lost **one** and then
  **three** packages during bring-up; at `2` they lost **none**.

The port is in the message, and that is the trap. `address: localhost:32890` is
the wait strategy naming what it is polling *for*; it is not the address that
failed. The call that times out is `GET /containers/<id>/json` on the docker
socket — the **daemon's own API**. The container is up and the port is mapped;
what cannot answer inside the 60 s budget is the daemon, and ~500 retries mean
the poll asked it for a minute and got nothing back.

The earlier reading quoted that line with the middle elided — `... ` in place of
`get state: Get ".../containers/<id>/json"`, which is exactly the segment that
names the daemon — and concluded the opposite: a wedged host route to an
already-published port, with concurrency ruled out. The four sweeps above refute
that. **The elision removed the evidence**, and everything downstream of it,
including a staged change lowering nothing, followed from the shortened quote.

So the primary control is **`INTEGRATION_PARALLEL`**, now `2`, which keeps the
daemon under its knee. A longer readiness timeout is still the wrong lever: it
waits out a daemon that is saturated rather than slow, and makes real failures
slower to arrive. `keeper/internal/integrationenv/start.go` is the second line
for the residual case, wrapping every bring-up in the tree:

```go
ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
	return tcpostgres.Run(ctx, "postgres:16-alpine", ...)
})
```

`integrationenv.SetupContext()` replaces the hand-copied
`context.WithTimeout(context.Background(), 90*time.Second)` each `TestMain` used
to carry, and sizes the budget for all attempts; `SetupContextFor(d)` is for a
suite whose wait strategy legitimately needs longer per attempt (the Redis
cluster forming quorum, a stand built on the spot).

Three properties are load-bearing, and each of them is a way this could have been
built wrong:

- **The retry is blind to why an attempt failed.** Deciding "infra, so retry"
  from the error text is the same string-matching
  [classify-l1-failure.py](../../scripts/classify-l1-failure.py) already has to
  maintain, and being wrong *here* silences a real failure instead of merely
  mislabelling one. A deterministic failure — a missing image, a bad request —
  still fails, three times over, a minute later.
- **It does not make red mean less.** A retry happens during setup, before the
  suite's first assertion, so no verdict is ever overwritten and no test is rerun
  on a whim (the thing NIM-393 exists to prevent). Every retry is logged, and the
  final error joins every attempt's error so the classifier still sees the
  container-layer markers it keys on.
- **Nothing chose to retry locally.** A per-package retry is a per-package
  decision, and a package that quietly opted out would reproduce NIM-569 while
  looking fixed. `TestContainersAreBroughtUpThroughStart`
  ([start_scope_test.go](../../keeper/internal/integrationenv/start_scope_test.go))
  parses every `integration`-tagged file in the tree and fails on any
  `testcontainers` bring-up that is not lexically inside a closure passed to
  `Start`. It is untagged on purpose, so `make check` runs it without docker, and
  it fails when it scans zero files — an empty scan is not a pass (NIM-238).
  The guard is itself tested (`start_scope_selftest_test.go`), which is not
  ceremony: its first version derived an unaliased import's identifier from the
  last segment of the path, so `github.com/testcontainers/testcontainers-go`
  resolved to `testcontainers-go` while every call site writes `testcontainers.`
  — it covered **zero** of the seven generic bring-ups and looked green doing it.

Two fixtures switch the retry off with `integrationenv.WithAttempts(1)`, and both
say why where they are used. The Redis cluster pins host ports 7000-7005 because
grokzen announces `127.0.0.1:<contport>` and the client dials what it is told, so
a replacement container asks for the *same* forward and terminate-then-rebind can
add `port is already allocated` to a failure that was already there. The L2 stand
publishes no port at all — both its wait strategies are `ForExec` — so the wedge
cannot happen to it, and retrying would only divide a context its caller sized
for a soul build, an image build and several applies. Neither is an exemption
from the convention: the call still goes through `Start`, so the scope guard
still sees it.

`make test-integration` now also passes `-timeout $(INTEGRATION_TIMEOUT)`, 15m.
The retry spends wall clock on the same budget the tests do, and Go's silent 10m
default was already thin for `internal/api` at 347-439 s — a package that tripped
it would print a goroutine dump, which reads as a hang rather than as a slow
suite. Stated rather than raised silently, so the ceiling is a number someone
chose. It is a per-**binary** clock, so `INTEGRATION_PARALLEL` of them run at
once and the two knobs are coupled: those 347-439 s were measured at `-p 4`, and
raising the parallelism slows the packages down without moving the ceiling.

The classifier learned three things in the same move, each of which had been
quietly costing a verdict:

- **`TIMEOUT` is its own verdict.** A killed binary names no failing test and
  prints no error string — a goroutine dump is function names and file paths —
  so it used to land in `UNCLEAR` reading "no test reported a failure", under
  several hundred lines of stack. It now says what happened and lists what was
  still running when the alarm fired.
- **The retry's own log lines are not evidence.** `Start` logs a recovered
  attempt, and that line carries the testcontainers error verbatim into the
  package's output — `check target: retries:` and `context deadline exceeded`,
  both of which the classifier keys on. A package that recovered from a blip and
  *then* failed an assertion was therefore reported as the container layer. The
  lines are dropped before matching; a bring-up that genuinely failed is
  unaffected, because the suite declares that in its own words.
- **`--- FAIL:` is matched per line.** The pattern was compiled without
  `re.MULTILINE`, so it only ever matched when the line happened to be the first
  thing after the previous package's `ok` — and under `-p 4` it usually was not.
  The visible symptom was a package with nineteen failing tests reported as
  `UNCLEAR`, "no test reported a failure": `REGRESSION`, the one verdict that
  says *fix the code*, was unreachable for most real logs.

`SOUL_STACK_INTEGRATION_START_ATTEMPTS=1` turns the retry off when you are
debugging a bring-up and want the first failure verbatim. Three is the default
because the wedge is rare (~1 start in 50 under a full sweep's load): it takes
the odds of losing a sweep to it from roughly one in two to one in a few
thousand, and a fourth attempt buys noise.

### Where `-race` runs and where it does not

The detector is bound to a **package set**, not to a job:

| Target | Package set | `-race` |
|---|---|---|
| `make test` | the untagged corpus, all modules (~155 pkg) | **no** — keeps `make check` runnable in ~90 s |
| `make test-race` | the **same** corpus | **yes** — blocking CI job `race` |
| `make test-integration` | only packages carrying `integration`-tagged tests (43) | **yes** |

Two mistakes this replaces, worth knowing because both read as their opposite:

- `-tags=integration` **widens** a package set, it does not narrow one. So L1 was
  running the whole untagged corpus under `-race`, in the same job that starts ~40
  container sets. Tests written against uninstrumented timing failed there on
  instrumentation speed — `TestRun_CancelDuringTask` lost ~1 % of runs under
  `-race` while staying green in `make test` on the same commit — so the job
  became unreliable in both directions at once (NIM-349). Fixing that means
  correcting the SET; `-race` was not removed from anywhere.
- Concurrent code lives in the untagged corpus: the async runner and its barriers
  ([ADR-0075](../adr/0075-intra-host-async-tasks.md)), the console pty pumps and write
  budget, the applybus fan-out. None of it carries an `integration` tag, so
  before the `race` job **no gate ran the detector over it at all** (NIM-312). A
  `test-race` target and a nightly job for it existed the whole time and produced
  nothing: the workflow had no schedule, and the job was `continue-on-error`.

`make test-race` passes `-count=1`, and that is load-bearing rather than tidy. A
race is found by observing an interleaving, so a green sweep means "no race was
observed this run" — a per-run claim. Cached, `go test` replays one historical
observation for ever: two consecutive sweeps of this target once finished in 8 s
reporting `(cached) ok` for all 140 packages, which is a gate that cannot fail
whatever the code does.

`make test-integration` also caps how many packages start containers at once
(`INTEGRATION_PARALLEL`, default 4) to keep that class of noise down, and sets
`SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1` — since NIM-238 that requirement is
the default anyway, and skipping it is the thing that has to be said out loud
(`SOUL_STACK_INTEGRATION_SKIP_DOCKER=1`).

### When a stand fails to build on docker registry credentials

A failure that names `docker-credential-…` is the machine, not the code, and it
picks out exactly the suites that **build** a stand from a Dockerfile — L2 in
`keeper/internal/trial` and L3b in `tests/e2e-live` — while every pull-based suite
on the same machine stays green. testcontainers resolves registry credentials for
each Dockerfile build, asking the configured helper about every registry in
`~/.docker/config.json`, and it treats a failure there as fatal; the image-pull
path logs the same failure and continues anonymously. So a helper that cannot run
takes down a build whose only base image is public and needed no credentials.

On WSL2 with Docker Desktop the helper is a Windows binary run through WSL interop,
so it stops working whenever interop is not registered — `systemd-binfmt` flushes
`binfmt_misc` and does not restore `WSLInterop`. Check with
`cat /proc/sys/fs/binfmt_misc/WSLInterop` (must print `enabled`) and re-register by
restarting WSL.

Neither harness depends on this any more: each probes the credential plumbing before
building and, when it is broken, builds without credentials and says so on stderr
(`keeper/internal/trial/l2_registry_auth.go` for L2, NIM-307;
`tests/e2e-live/harness/registry_auth.go` for L3b, NIM-347). That costs nothing,
because a helper that cannot run yields credentials for no registry anyway.

The two are deliberate copies, each with its own guard tests. They cannot share
code: `keeper/internal/` is unreachable from another module, and `tests/e2e-live`
depends on no Go module of ours except `proto` — lifting ~40 lines into a new
workspace module would make four test modules require and replace it. A divergence
therefore surfaces as a failing guard rather than as one harness quietly losing the
fix.

## Local live-gate of large features (`make e2e-live-gate`)

`e2e-live-gate` - **mandatory local live run before batch commit of each
major feature**, and since NIM-879 the blocking job `release.yml` runs on a tag: goreleaser
declares `needs: live-gate`, so a red tier produces no release artifacts. This is a curated
subset of L3b (~15-25 min, docker required), not all `make e2e-live` — that one is the broad
tier, dispatched from `nightly.yml`, which has no schedule. The point of the gate is to prove
on a real `soul` binary that the key mechanics are alive before the edits go to
commit.

**What runs** (build-tag `e2e_live`: real `soul` in privileged Debian-12
container + Keeper process on host + mTLS + live apply). The list lives in
`E2E_GATE_TESTS` in the [Makefile](../../Makefile) and is not repeated here — it
feeds the `-run` mask, the per-test `--- PASS` guard and the classifier from one
place, and a copy in prose is a copy that goes stale (it named three of the nine
for a while). Entries there must be **exact** test names: the mask tolerates a
prefix, the other two readers do not, and `make check` now refuses a list whose
entries are not tests the suite really has
([scripts/e2e-gate-mask.sh](../../scripts/e2e-gate-mask.sh)). Four groups:

- **Delivery mechanics `SoulModule`** (fixture `tests/e2e-live/module-delivery-live`,
`TestL3bModuleDeliveryLive_*`) - flagship check for which the gate was opened:
synthesis of the module installation step from `service.yml::modules[]` to
[ADR-065](../adr/0065-core-module-installed.md) → `FetchModule` with Keeper →
Sigil-verify signatures → hot-register in the Soul registry → live performance
delivered module vs real redis.
- **Basic apply-smok nginx** (`TestL3bSmokeNginxLive_*`): `core.pkg` apt-install +
`core.service` systemd-start on a live host - checks that normal apply is not broken.
- **Smoke plugin channel** (`TestL3bPluginChannel_*`): module directory + allow mechanics
gRPC-stdio plugin channel.
- **A service brought to a working state, then operated** (`TestL3bRedisServiceLive_*`:
create-from-souls, day-2 add-user) — the claim the three bullets above cannot make. They
prove a module is **delivered**; these prove a **service** is stood up and then changed:
state written and read back, secrets minted at keeper-derived Vault paths, `users.acl`
re-rendered whole, the live ACL reconciled through the plugin channel. The subject is out
of tree and pinned by commit (NIM-876) — `examples/service/redis` used to play this part
and left with NIM-871, taking six `TestL3bRedisLive_Day2*` cases with it. Four of those six
claims (update-config, restart, destroy, rotate-tls) are still uncovered, as is the machine
half of a create; see "A service create is NO LONGER locally covered" below.

**When required.** Before a batch commit of a feature that meets the "major" criterion -
same triggers that escalate to architect: **>5 files are affected** OR being corrected
**key nodes** (Keeper↔Soul contract, plugin infrastructure, render/dispatch pipeline,
`state_schema`, template engine). The gate does not require minor adjustments. `make check` gate
**doesn't** include (it's docker-free); full `make e2e-live` is dispatched from `nightly.yml`.

**How ​​to launch:**

```bash
make e2e-live-gate
```

Target itself collects native `keeper` (`make build` - harness launches Keeper on
host) and linux-`soul` (`make build-linux` - mount to container); plugin
`redis` collects the test itself. `E2E_KEEPER_HOST` (IP on which
soul-container calls Keeper-on-host) **auto-detected by target** via
`hostname -I`. On **WSL2** this matters: the container cannot reach `localhost`,
so a LAN IP is required. Override manually - `make e2e-live-gate E2E_KEEPER_HOST=<ip>`.

**Run in isolation** where you can: L3b tests raise docker containers (keeper +
PG + Redis + Vault + soul), and on WSL2 competing docker/build load stretches
bring-up. Measured over four gate runs (NIM-406): 503s on an idle box against
662-805s at a loadavg of 20-42.

Slower is not the same as red, and the difference matters more than the
advice. In that same measurement the gate came back **green at a loadavg of
20**, and neither red run failed bring-up at all - both died inside a test
reaching the internet (`core.url` on github.com, `core.pkg` on
deb.debian.org; NIM-542). So a red gate is not evidence that the box was
busy, and rerunning it quietly until it passes buries exactly the failures
worth reading. What actually failed is a question with an answer - see below.

The github.com half of that is now gone: the gate serves those release tarballs
from a local mirror and no longer downloads them per run (NIM-542, below). The
apt half is not — `deb.debian.org` and `packages.redis.io` are still dialed on
every live `create`.

### Reading a red gate (NIM-406)

A failure while the **stand** comes up and a failure of an **assertion** arrive in
the same `--- FAIL: TestX` shape and have opposite answers. On a red gate the
target runs
[`scripts/classify-e2e-live-failure.py`](../../scripts/classify-e2e-live-failure.py)
and labels every gate test:

- **STAND-SETUP** — the harness itself declared that bring-up failed, so no
  assertion in that test ever ran and nothing in its output is a statement about
  the code. Rerun that one test alone. If it fails the same way twice on an idle
  machine, the machine is the finding.
- **TEST-FAILURE** — anything else that failed: the test got as far as asserting,
  or it died somewhere nothing declared. Treat it as a finding.
- **NOT-RUN** — skipped or never started (a missing `keeper`/`soul-linux` binary
  skips; a whole-run timeout kills the rest). Never a pass: the `--- PASS` guard
  fails the gate for exactly these.

Unlike the L1 classifier this one carries **no signature lists**. `NewStack` and
`BuildCommunityRedisPlugin` defer `declareStandSetupFailure`, so the harness
*states* which layer died instead of the reader guessing it from library text —
and guessing is what produces a false infra label. Forgetting the defer therefore
fails safe: that entry point's failures read as TEST-FAILURE, and the mistake
costs someone a look at code that turns out to be fine.

The opposite mistake — an infra label on a real regression — is the one that
makes a regression disappear, and dropping the signature lists does **not** put it
out of reach. It stays reachable through the mechanism's own moving parts, and
each of the three below was wrong at some point:

- **Where the declared region ends.** The defer covers a *range* of the entry
  point, and everything in that range is claimed to be infrastructure. The first
  cut set the flag on the last line of `NewStack`, putting `keeper init`,
  `keeper run`, `POST /v1/services` and the whole Soul onboarding path inside it
  — and that onboarding path runs in no other suite. A regression there would
  have printed STAND-SETUP on all nine gate tests, which reads as a bad day for
  docker. Pinned by `TestDeclaredRegionsEndBeforeTheProductRuns`, which parses
  the harness and fails if a call that runs this repo's own binaries falls
  between the defer and `infraUp = true`.
- **Which test a log line belongs to.** The classifier reads the marker out of a
  per-test block. Keyed on `=== RUN` alone it followed only the most recently
  *started* test, so interleaved output moved the marker onto a neighbour —
  labelling the bring-up failure TEST-FAILURE and the real regression
  STAND-SETUP, both wrong, in one step. It now switches on `=== CONT` and
  `=== NAME` as well and attributes each result by the name on its own line;
  `--self-test` pins the interleaving.
- **What the region is allowed to touch.** A region may end in the right place
  and still make the wrong kind of claim. NIM-377 deleted the plugin's
  `manifest.yaml`; the harness read it from the repo tree *inside* the region, so
  a deleted file in this repository printed STAND-SETUP on all nine gate tests
  and the gate stayed unpassable while reading as a machine problem (NIM-515).
  The same guard now also fails when a declared region reaches `repoRoot`,
  directly or through a helper.

Note what that third one cost to find: the guard already existed, and it looked
for a **list** of names — the offending call named none of them. So the honest
statement is not that the mislabelling is now unreachable, or reachable only
these three ways. It is that each way found so far is held by a check that runs
in `make check`, the newest of them by a property rather than a list, and that
the list of ways is open until something closes it. Treat an edit to any of the
three as an edit to the gate's meaning.

The labelling downgrades nothing — the gate still exits non-zero, and a
STAND-SETUP that survives a solitary rerun is a finding whatever it was labelled.
No readiness wait was loosened to get here either; NIM-406 made them **stricter**
(each stand now waits for its own mapped port, and vault for an unsealed API) on
one shared, explicitly named budget.

One thing to know before touching those numbers: the budget a container really
gets is the **smallest** bound above it, and there are three stacked (the check's
own, the set's deadline, and `NewStack`'s ctx over all three stands in sequence).
Raising the per-container budget without the outer one silently gives the last
stand in line whatever the first two left, and it then fails as a parent-context
deadline that names nobody. So `standBringUpTimeout` is **computed** from
`standReadyTimeout`, never written beside it, and `waitstrategy_test.go` fails if
the outer bound drops below what its contents can ask for.

There is a **fourth** bound above all of them — `go test -timeout 45m` on the gate
— and it is deliberately *not* derived from the three, which is worth saying
because the rule just above invites the opposite. It is not the same kind of
bound. The stacked three reallocate silently: the last stand in line gets the
remainder and fails without naming itself. `-timeout` is loud — it kills the
binary with a panic naming the test it was in, tests that already reported keep
their printed verdicts, and tests that never started are labelled NOT-RUN. So its
correct value is "comfortably above an honest run", not "above the worst case its
contents can ask for": an honest gate run is ~10-15 min (a passing test costs
44-105 s), and reaching 45 m needs six consecutive full-budget bring-up timeouts
(6 × `standBringUpTimeout` = 42 min) — a machine so broken that the gate is
comprehensively red either way and the last verdicts buy nothing. Sizing it to
that sum would only mean a genuinely hung run burns an hour before anyone hears
about it, which is the one job this bound has.

**One test is not covered by that, and it is the interesting one.** The test
running when the wall hits gets no result line, and its label is a guess. Go's
timeout panic comes from a watchdog goroutine (`time.AfterFunc` in
`(*M).startAlarm`), so the process dies without unwinding the test goroutine —
`declareStandSetupFailure` is a `defer` and never fires. A stand that hangs
forever therefore produces no marker, and `classify()` falls through to
TEST-FAILURE. That is the safe direction (someone looks) but it is not knowledge:
the layer is genuinely unknown there, and the tool currently reports it as if it
were not. A distinct TIMEOUT verdict is tracked in **NIM-511**; until then, read
"TEST-FAILURE on the test the panic names" as "unknown", and check whether the
gate's `-timeout` was the thing that expired before treating it as a finding.

### What the gate downloads (NIM-542 → NIM-876)

The classifier above answers "bring-up or the code?", and for a while there was a
third answer it could not give, because the gate had a third way to fail: the
network. Six of the then-nine gate tests ran a live service `create`, and that create
fetched `node_exporter`, `redis_exporter` and `vector` from **public github.com** from
inside the container — about 18 downloads per gate run. A blocking pre-tag step whose
acceptance is "three runs on an unchanged slice give the same result" was resting on
something that is not in the slice, and both red runs measured above died exactly there.

NIM-542 answered that with a local mirror: a digest-verified cache outside the repo, an
HTTPS server over it, and a `vars/99-*` layer written into the **fixture's** copy of the
service. NIM-871 then cut the service those six tests created, and the guards that held the
mirror's pin table against the service's own declarations went with it — leaving an
unverified copy of a service that existed nowhere.

**NIM-876 removed the mechanism rather than leaving the copy to rot.** The way to make such
a table true is to ask what the subjects declare, and no live subject declares a release
fetch: the in-tree fixtures download nothing, and the pinned out-of-tree service installs
from an apt repository, which is a different mechanism. What replaced it is a docker-free
guard (`harness/upstreamfetch.go`) that fails the moment any live subject declares
`default(vars.<prefix>_base_url, 'http…')` again — so the absence stays a decision instead
of turning back into an outbound dependency nobody notices. The mirror is in git whole, at
the commit that removed it, for the subject that legitimately needs one.

So no fourth verdict was added, and deliberately: the category the gate would have needed
it for does not occur inside the gate. The one test that kept the real github path covered
(`TestL3bRedisLiveUpstream_ArtifactsFromGitHub`, deliberately outside `E2E_GATE_TESTS`, as
a blocking gate must never depend on something outside the repository) ran
`examples/service/redis` and left with it in NIM-871.

**What the gate DOES download, on purpose:** the subject itself. A service is its own
repository, so the two service tests run `github.com/soul-stack-services/redis` at a commit
pinned in `tests/e2e-live/harness/servicecatalog.go`, fetched once into a cache outside the
repo and extracted to a path that carries the commit. The pin is what makes the run
reproducible; the cache is what makes it offline after the first fill. Neither alone would
do: without the pin the gate proves whatever is in some directory, and without the cache
its verdict is again a question about github.com. `make e2e-live-services` primes it, and
`SOUL_STACK_E2E_SERVICE_OFFLINE=1` is how "needs nothing from the network" is demonstrated
rather than asserted.

**Still not hermetic:** `core.pkg.installed` reaches `deb.debian.org` and
`packages.redis.io` on every live create. NIM-542 scoped the tarballs only, and NIM-876
did not widen that scope.

The wall itself is also not derived from anything, and the two runs disagree:
`make e2e-live` gives the whole package 30 m while the gate gets 45 m, so the larger
run has the tighter budget. Tracked in **NIM-512**.

**What it DOESN'T cover** (this is stand/cloud/PHASE 2/L3c-k8s, not local gate):
cloud provision (`CloudDriver`), `install_method=binary` (there is no public source of
standalone redis binaries), cluster redis topology, multi-keeper HA.

**★ A service create is covered again, by a subject that is not in this tree (NIM-876).**
Each of the six `TestL3bRedisLive_Day2*` tests ran `examples/service/redis::create` end to
end first — a real `redis-server` plus the node-exporter / redis-exporter / vector
destinies into a Debian-12 container — and only then exercised its day-2 scenario against
that live instance. That service left the engine with NIM-871 and the six tests went with
it, taking the gate from nine tests to three.

Two of those claims are back as `TestL3bRedisServiceLive_{CreateFromSouls,Day2AddUser}`,
driving `github.com/soul-stack-services/redis` at a pinned commit. Three things are still
NOT covered, and each for its own reason:

- **four day-2 scenarios** — `update_config`, `restart`, `destroy`, `rotate_tls` — because
  the published service does not have them yet;
- **the machine half of a create** — VMs through a keeper-side provider, cloud-init, an
  agent installed over `core.ssh.run`, a bootstrap token redeemed — because a docker tier
  whose souls are already onboarded cannot host it. A hand-run workstation stand covers it,
  and nothing automated does;
- **the exporters and vector** — the published service deploys the redis brick alone, so
  the three-destiny composition the old create exercised has no live test.

This used to be what the examples corpus was FOR: e2e ran against `examples/service/*`, so
a green gate guaranteed the documented example still worked. That guarantee had one hard
prerequisite — **a subject a live test drives must install from PUBLIC sources only**.
Point one at an internal mirror and the gate stops being runnable, the red goes unnoticed,
and the subject rots invisibly (exactly what happened between `22130c2b` and NIM-208, when
the redis service pointed at an internal Nexus placeholder and nobody could run it). The
rule did not go away with the corpus — it moved onto the pinned service, where it is
enforced by whoever bumps the pin, and by `make e2e-live-gate` refusing a commit it cannot
fetch.


## Documents by level

- [e2e.md](e2e.md) - regulatory spec L3a harness: fixtures/expectations format,
how to add new E2E cases, soul-stub contract. Includes L3b partition
(real-soul-in-container, link to `tests/e2e-live/README.md`) and L3c section
(kind + bitnami Helm, link to `tests/e2e-k8s/README.md`).
- [e2e-cloud.md](e2e-cloud.md) - cloud live-E2E orchestrator runbook
(`scripts/e2e-cloud/`): bash over Operator API via teleport, two keeper worlds
(`local` / `tsh`), suites create / create-destroy / day-2, asserts by apply_run,
report format and exit codes. L4-adjacent, outside the ephemeral-invariant L3a/L3b.

## See also

- [ADR-023 - Trial and DSL-coverage](../adr/0023-trial-test-runner.md) - L2.
- [ADR-039 - E2E three levels](../adr/0039-e2e-testing.md) - L3a/L3b/L3c.
- [dev/local-setup.md](../dev/local-setup.md) - testcontainers-go and docker-compose dev stack (L1-infra).
