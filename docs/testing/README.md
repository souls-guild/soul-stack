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
| **L2** - Trial | `examples/destiny/<name>/_trial/`, `examples/service/<name>/scenario/<n>/tests/` | no | Hermetic prerender + migration-assert on fixtures (`soul-trial`). | each PR via `make build` + `soul-trial` |
| **L3a** - E2E fast-loop | `tests/e2e/` | `e2e` | testcontainers (PG/Redis/Vault) + Keeper-process in-process + soul-stub. Contract tests apply_runs lifecycle / RBAC / audit / MCP. | every PR (when L3a-imp slice stabilizes), `make e2e` |
| **L3b** - E2E smoke | `tests/e2e-live/` | `e2e_live` | Real `soul`-binary in a privileged Debian-12 container (systemd-PID-1) + Keeper process + mTLS + real apply. Flagship scripts. | nightly/on-demand; **feature-complete** (5 slices L3b-1..L3b-5 = done): real CSR Bootstrap + `smoke-nginx-live` (apt + systemd) + multi-host `redis-cluster-live` (3 containers); `make e2e-live` really drives nightly. L3b-6 (drift-live) - done: `TestL3bDriftLive_HelloWorld` runs drift-check on a live soul through a real `core.file.Plan` (module `core.file.present`), and not stub-Plan like L3a |
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
`check-all` runs it. L3b stays outside both — CI does not run it either.
One difference from CI survives on purpose: CI gives each tier its own runner,
`check-all` gives them one docker daemon and starts L3a right after ~300
container-starting L1 packages. A failure at container startup there is that
contention — rerun the package alone before believing it, and do not loosen a
readiness wait to make it go away.
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
Measure release readiness with `check-all` (or a CI run over the pushed branch),
never with `check` alone - and read a CI result **per sha**, not per branch: a
run superseded by the next push is reported as `cancelled`, which sits next to
`failure` in the list and next to `success` in memory.

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
isolation rather than rolling the change back.

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

`make test-integration` also caps how many packages start containers at once
(`INTEGRATION_PARALLEL`, default 4) to keep that class of noise down, and sets
`SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1` — since NIM-238 that requirement is
the default anyway, and skipping it is the thing that has to be said out loud
(`SOUL_STACK_INTEGRATION_SKIP_DOCKER=1`).

## Local live-gate of large features (`make e2e-live-gate`)

`e2e-live-gate` - **mandatory local live run before batch commit of each
major feature**. This is a curated subset of L3b (~15-25 min, docker required), not
all `make e2e-live` (that one remains nightly / pre-release). The point of the gate is to prove
on a real `soul` binary that the key mechanics are alive before the edits go to
commit.

**What runs** (build-tag `e2e_live`: real `soul` in privileged Debian-12
container + Keeper process on host + mTLS + live apply). Mask -
`TestL3bModuleDeliveryLive|TestL3bSmokeNginxLive|TestL3bPluginChannel`:

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

**When required.** Before a batch commit of a feature that meets the "major" criterion -
same triggers that escalate to architect: **>5 files are affected** OR being corrected
**key nodes** (Keeper↔Soul contract, plugin infrastructure, render/dispatch pipeline,
`state_schema`, template engine). The gate does not require minor adjustments. `make check` gate
**doesn't** include (it's docker-free); full `make e2e-live` - nightly / pre-release.

**How ​​to launch:**

```bash
make e2e-live-gate
```

Target itself collects native `keeper` (`make build` - harness launches Keeper on
host) and linux-`soul` (`make build-linux` - mount to container); plugin
`community.redis` collects the test itself. `E2E_KEEPER_HOST` (IP on which
soul-container calls Keeper-on-host) **auto-detected by target** via
`hostname -I`. On **WSL2** this is critical: `localhost` is not visible from the container, it is needed
LAN-IP. Override manually - `make e2e-live-gate E2E_KEEPER_HOST=<ip>`.

**Run in isolation**, without parallel docker/build load: L3b tests
raise docker containers (keeper + PG + Redis + Vault + soul) also on WSL2
sensitive to competitive docker load - when running in parallel with another
heavy docker/build work may cause the Vault container to fail to rise (connection
refused). When raising containers (infrastructure, not code regression) -
restart the gate.

**What it DOESN'T cover** (this is stand/cloud/PHASE 2/L3c-k8s, not local gate):
cloud provision (`CloudDriver`), `install_method=binary` (there is no public source of
standalone redis binaries), cluster redis topology, multi-keeper HA.

**Redis-create IS locally covered.** Each of the six `TestL3bRedisLive_Day2*` tests runs
`examples/service/redis::create` end to end first - installing a real `redis-server`
plus the node-exporter / redis-exporter / vector destinies into a Debian-12 container -
and only then exercises its day-2 scenario against that live instance. Sentinel mode
with `replicas_per_master: 0` (a standalone-equivalent) is what they create.

This is what the examples corpus is FOR: e2e runs against `examples/service/*`, so a
green gate is the guarantee that the documented example still works. That guarantee has
one hard prerequisite - **the examples must install from PUBLIC sources only**. Point one
at an internal mirror and the gate stops being runnable, the red goes unnoticed, and the
example rots invisibly (which is exactly what happened between `22130c2b` and NIM-208:
`examples/service/redis` pointed at an internal Nexus placeholder and nobody could run
it). `examples/service/redis` therefore installs from the official Redis apt repository
by default; a fleet on an internal mirror overrides `essence.install_package` in
`spec.essence`.

`TestL3bRedisLive_CreateStandalone` remains skipped for an unrelated reason: it targets
the `standalone` redis_type removed in 2026-06-25, not any coverage gap.

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
