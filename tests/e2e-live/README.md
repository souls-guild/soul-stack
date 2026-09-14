# tests/e2e-live — L3b real-soul-in-container (ADR-039)

An L3b smoke loop with a **real** `soul` binary in a Linux container (Debian-12
systemd-PID-1). Unlike L3a (`tests/e2e/`, a soul-stub helper package), L3b:

- uses a real `soul` binary inside a privileged container with systemd-PID-1;
- goes through the real CSR Bootstrap flow (CSR → `Keeper.Bootstrap` → leaf cert);
- a real `core.pkg.installed name=nginx` installs a real package;
- a real `core.service.running name=nginx` starts a real service through systemd.

## Running

```sh
# Two pre-requisites, and they are different binaries:
make build-linux    # soul-linux-amd64, mounted into the container
make build          # keeper, run NATIVELY on the host by the harness

# Full L3b run (depends on both of the above)
make e2e-live

# Or a single test directly. Do the two builds first: a missing binary SKIPS
# (reads as `ok`), and only a stale KEEPER fails — see the two sections below.
SOUL_BIN_LINUX=$(pwd)/soul/bin/soul-linux-amd64 \
  go test -tags=e2e_live -run TestSmokeNginxLive ./...
```

Requires **docker** with support for privileged containers (cgroup-mount + systemd-PID-1).

### The keeper binary must be this tree (NIM-490)

L3b runs the keeper as a host process from `keeper/bin/keeper` — a file, not
something the harness compiles. `NewStack` now asks it which commit it carries
(`keeper version`, stamped from `git describe`) and **fails** when the answer is
not this tree, or when a keeper-side source is uncommitted and younger than the
binary. The refusal is a declared bring-up failure (`STAND-SETUP`), placed below
the declaration and above `infraUp = true`, so it lands before the containers are
paid for and does not read as a finding about the code; `provenance_test.go`
guards both ends.

Absence is still a **skip** here, and that asymmetry with L3a — where NIM-533
made a missing binary fatal, on the grounds that a tier which ran nothing must
not report `ok` — is a known gap, not a decision: NIM-625. It is out of
NIM-490's scope so the two changes stay separable. Staleness is a failure
regardless: a tier that silently reports on last week's build is worth less than
no tier.

The trap this closes is L3b-specific and was undocumented: `make e2e-live` used
to depend on `build-linux` **only**, which produces `keeper-linux-amd64` for the
soul container and never touches the native keeper the harness actually spawns.
Rebuilding the obvious thing did not fix the run. The target now depends on
`build` as well, and the failure message says outright that `make build-linux` is
not the remedy.

### The soul binary is checked for presence only (NIM-636)

The other pre-requisite, `soul/bin/soul-linux-amd64`, gets no such treatment: the
harness checks that the file exists and mounts whatever `make build-linux` last
produced, however old. `make e2e-live` depends on `build-linux`, so the common
path rebuilds it — a direct `go test -tags=e2e_live` does not, which is the
invocation the section above exists to make safe. The keeper's mechanism does not
transfer as-is (different source roots), so it is NIM-636 rather than a one-line
addition here.

### WSL2 + Docker Desktop

On native Linux, the soul container dials into keeper via
`host.docker.internal` (host-gateway) — the default, nothing to configure.

On **WSL2 + Docker Desktop**, containers live in the DD-VM, while the keeper process
runs in the WSL2 distro (different network namespaces). From the container, `host.docker.internal`
resolves to the DD-VM gateway (`192.168.65.254`), where keeper is NOT listening →
bootstrap fails with `connection refused`. The real WSL2 host IP is reachable
from the container — pass it via `E2E_KEEPER_HOST` (the harness wires it into the
`soul.yml` endpoint, ExtraHosts, and the keeper cert's **TLS-SAN**):

```sh
make build-linux
cd tests/e2e-live
E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') \
  go test -tags=e2e_live -run TestL3bSmokeNginxLive -timeout 25m
```

Without `E2E_KEEPER_HOST` behavior doesn't change (CI default `host.docker.internal`).

## Upstream release artifacts (NIM-542)

A service create deploys three upstream binaries — `node_exporter`, `redis_exporter`
and `vector` — each from a pinned GitHub release tarball. Six of the then-nine
`make e2e-live-gate` tests ran such a create, so the blocking pre-tag gate used to
pull ~18 tarballs from **public github.com**, from inside the soul container, on every
run. The gate's acceptance is "three runs on an unchanged slice give the same result";
github.com is not in the slice.

> ★ Those six tests ran `examples/service/redis`, which NIM-871 cut out of the engine,
> so **no gate test drives this mirror today**. The machinery below stays wired and
> primed — every `NewStack` still fills the cache — because the out-of-tree service
> repo that inherits the create will need it. What did NOT survive is the set of guards
> that kept `artifactCatalog()` honest against the service's own pins; see the header of
> `harness/artifactcatalog.go`.

The harness serves those tarballs itself:

1. `ensureArtifactCache` fills a cache **outside the repo and outside `$TMPDIR`** —
   `$SOUL_STACK_E2E_ARTIFACT_CACHE`, else `$XDG_CACHE_HOME/soul-stack/e2e-live/artifacts`.
   It must survive `git clean` and a reboot, or "hermetic" would only mean "downloads
   once per run". Entries are digest-verified on the way in, and a wrong-digest file is
   deleted rather than reused. That verification is the harness's own, and it has to be:
   `vector` and `redis-exporter` pass `checksum: "${ input.sha256 }"` to `core.url` and
   would reject bad bytes inside the container, but `node-exporter/tasks/install.yml`
   declares no checksum by design, so a truncated node_exporter tarball would pass the
   fetch and arrive as a `--- FAIL` somewhere later — unpack, or a service that never
   comes up — reading as a defect in the code.
2. `startArtifactMirror` raises a read-only **HTTPS** server over that cache on an
   **ephemeral port**, advertised at the address the container reaches the host on
   (`keeperEndpointHost()`). The cache is laid out exactly like upstream —
   `<prefix>/v<version>/<file>` — so only `base_url` has to change.
3. `addArtifactMirrorLayer` writes `vars/99-e2e-live-artifact-mirror.yaml` into the
   **fixture's materialized copy** of the service, setting `<prefix>_base_url` at the
   mirror and `<prefix>_allow_private: true` (the mirror is on a private address, and
   the SSRF guard is on by default).

**Why HTTPS and not HTTP.** All three destinies declare `base_url` with
`pattern: "^https://…"`, because `core.url` refuses plain http without an explicit
opt-out — transport security is a property of the artifact under test. The first cut of
this mirror served http and died inside the container on `input $.base_url … does not
match pattern`; the fix belonged on the fixture's side, not in the destiny. So the
mirror generates a throwaway CA and leaf per run (`generateArtifactMirrorTLS`, SAN =
whatever the container will dial — an IP on WSL2, a name on native Linux), and
`SpawnSoulContainer` drops that root into `/usr/local/share/ca-certificates/` and runs
`update-ca-certificates` **before the soul is started**. The product's fetch path —
scheme check, TLS handshake, chain validation — runs exactly as it does against github.

**The layer goes into the copy, never into the service tree.** A shipped example is
the subject of these tests (NIM-211); bending it to suit the fixture would leave the
gate green about a service nobody runs. What the fixture does here is exactly the
override a service's `vars/00-base.yaml` documents for an operator with an internal
mirror.

The layer is written only for the prefixes the materialized service actually reads,
scanned out of its own `scenario/` tree (`scenarioArtifactPrefixes`) — a
`smoke-nginx-live` stand fetches nothing and gets no layer.

```sh
# Prime the cache deliberately (idempotent; also the gate's first step)
make e2e-live-artifacts

# Then the gate needs no outbound access for these artifacts at all
SOUL_STACK_E2E_ARTIFACT_OFFLINE=1 make e2e-live-gate
```

`SOUL_STACK_E2E_ARTIFACT_OFFLINE=1` turns a cache miss into a loud error instead of a
silent fetch. That is the switch the "no outbound access" property is *checked* with —
not a mode to run in normally.

### How this stays honest

An override three YAML layers away from where it is read is a claim, and claims like
that hold until they don't: rename the file, add a `vars/_stack.yaml` to the example,
rename a var, and the layer contributes nothing at all — the create still passes, from
github, and the gate is quietly back on the public internet with a green result.

So the mirror **counts what it served**, and `Stack.Cleanup` fails the test if a
successful run never asked it for an artifact it had redirected. Alongside it there used
to be docker-free guards in `harness/artifactcatalog_test.go` that failed twenty minutes
earlier if the service grew a fourth external fetch, bumped a version or digest the
catalog does not carry, gained a vars layer that out-sorted `99-*`, or declared a
`_stack.yaml`. ★ **Those six guards re-read `examples/service/redis` and left with it
(NIM-871)** — none of that is checked today.

One guard of that family survives, because its subject is a destiny rather than the
deleted service: `TestArtifactMirrorURLIsOneTheDestinyWillAccept` generates the overlay
this fixture would write and matches it against the `pattern:` the destiny declares — both read at
run time, neither hardcoded. That is the guard the http/https mistake above walked
straight into, and it now costs a second instead of a live run.

### The real github path is no longer covered

Hermetizing the gate moved a real dependency out of it, and a dependency nobody
exercises rots. `TestL3bRedisLiveUpstream_ArtifactsFromGitHub` was the answer: the same
create with `harness.Config{UpstreamArtifacts: true}` — tarballs straight from GitHub
Releases, the way an operator's first run gets them — deliberately **outside**
`E2E_GATE_TESTS`, as the one test in the tier allowed to fail for a reason outside the
repository.

It ran `examples/service/redis` and left with it (NIM-871). The harness half survives
and is still worth reusing: `harness.RequireUpstreamArtifacts` probes the real URLs
*before* the stand and skips naming the host — nothing of this repository has run yet,
so the failure cannot be about this repository — and
`harness.ReportUpstreamIfItWentAway` re-probes *after* a failure to print either "read
the failure above as environment" or, the more valuable half, "upstream is still
reachable, so this is a finding". Nothing calls either today.

### What is still not hermetic

`core.pkg.installed` still reaches `deb.debian.org` and `packages.redis.io` (redis-tools,
redis-server, redis-sentinel, plus smartmontools/nvme-cli/ipmitool for node_exporter's
default collectors). NIM-542 scoped and fixed the github.com tarballs; **apt is untouched**,
so "three runs on an unchanged slice agree" is closer but not yet reachable offline.

## Frequency

- **L3a** (fast loop) — every PR via `make e2e` (~30-60 sec per test).
- **L3b** (smoke loop) — **nightly cron / on-demand** (~5-15 min per test).
- **L3c** (k8s loop) — weekly / pre-release.

## Layout

```
tests/e2e-live/
├── README.md
├── go.mod                          # separate go module (deps don't leak)
├── cmd/artifact-cache/             # `make e2e-live-artifacts` — primes the tarball cache
├── dockerfiles/
│   └── debian-12.Dockerfile        # privileged systemd-PID-1 base image
├── harness/                        # copy of L3a harness + L3b-specific helpers
│   ├── artifactcatalog.go          # the pinned upstream tarballs + the vars overlay (NIM-542)
│   ├── artifactmirror.go           # local cache + HTTP mirror + upstream probe (NIM-542)
│   ├── stack.go                    # NewStack — PG/Redis/Vault/keeper
│   ├── bootstrap.go                # IssueBootstrapToken
│   ├── container.go                # SoulContainer + SpawnSoulContainer (privileged Debian-12)
│   ├── asserts.go                  # keeper-side + container-side asserts (AssertHost*)
│   ├── expectations.go             # LoadExpectations / AssertExpectations (+ host_state)
│   ├── coven.go                    # AddMember (roster via incarnation_membership)
│   ├── seed.go                     # direct-SQL seeds + CreateIncarnationOnRoster (bootstrap order)
│   ├── operator.go                 # Operator API HTTP client
│   ├── vault.go                    # InitVaultTestSecrets / IssueKeeperServerCert / SeedVaultKV
│   ├── config_builder.go           # buildKeeperYAML
│   ├── probe.go                    # waitForReady
│   ├── git.go                      # SetupGitRepo
│   └── destiny.go                  # MaterializeDestinies
├── smoke-nginx-live/  staged-probe-live/  when-gate-live/  module-delivery-live/
│   └── …/expectations/             # per-example host_state expectations
├── smoke_bootstrap_test.go         # TestL3bBootstrap_OneSoul
├── smoke_nginx_live_test.go        # TestL3bSmokeNginxLive_InstallAndStart
├── module_delivery_live_test.go    # TestL3bModuleDeliveryLive_SynthesisFetchHotRegister
├── plugin_channel_test.go          # TestL3bPluginChannel_CatalogAndAllow
├── staged_probe_live_test.go       # TestL3bStagedProbeLive_WhereTargetsOnlyMaster
└── plugin_beacon_test.go           # TestE2EBeaconPlugin_FullLoop
```

## Bootstrap order of a live stand (NIM-192)

A test that brings up a NEW incarnation on already-onboarded Souls goes through
`Stack.CreateIncarnationOnRoster` and never hand-rolls the order:

1. seed the `incarnation` row (`status='ready'`, empty spec/state) — direct SQL;
2. bind each Soul as a member (`incarnation_membership`);
3. wait for each member's first `SoulprintReport`;
4. run the create scenario as an ordinary explicit run.

Two constraints pin that order and admit no other:

- **Membership cannot come first.** Since NIM-124 membership is a first-class
  relation with FK `incarnation_membership_incarnation_fk` → `incarnation(name)`
  (migration 099). Binding before the row exists is `SQLSTATE 23503`.
- **The run cannot come first.** A create run that rolls onto a ready roster
  (`provision: {enabled: false}`) resolves the roster at run start, so an unbound
  roster aborts with `no_hosts` before dispatch (`run.go` §3).

`POST /v1/incarnations` inserts the row **and** starts the create run in the same
call (`lifecycle.auto_create`), leaving no window between them — hence the direct
seed. The consequence to keep in mind when writing expectations: create is an
explicit run here, so it writes `incarnation.scenario_started`, **not**
`incarnation.created`. POST's own create path stays covered by the handler unit
tests and, for the BARE variant (no starting scenario), by L3a (`tests/e2e`) —
L3a hit the same FK ordering and moved to the same helper (NIM-210), so the
auto-started create run is no longer covered end-to-end at either tier.

`AddMember` on its own remains correct for an incarnation that already exists
(e.g. `fc5_when_gating_test.go`, which seeds a ready incarnation and runs a
day-2 scenario); it fails fast with the order instruction if the row is missing.

## Slices

L3b is implemented iteratively. Slice map (architect consultation `a0af3d90ec118aafd`):

| Slice | Content | Status |
|---|---|---|
| **L3b-1** | Layout + Dockerfile + `make build-linux` + `harness.NewStack` skeleton (PG/Redis/Vault/keeper, WITHOUT soul-container). | done |
| **L3b-2** | Real Bootstrap flow: `IssueBootstrapToken` + `SpawnSoulContainer` (privileged Debian-12 + soul-binary mount + CSR Bootstrap RPC). | done |
| **L3b-3** | First L3b example `smoke-nginx-live` (actually installs nginx via apt + systemctl start). | done |
| **L3b-4** | Container-side asserts (`AssertHostPkgInstalled` / `AssertHostServiceActive` / `AssertHostFileExists` / `AssertHostFileContent`). | done |
| **L3b-5** | Multi-host (`redis-cluster-live` with 3 soul containers) + YAML expectations loader (`harness.LoadExpectations` / `Stack.AssertExpectations`). The multi-host FIXTURE left with `examples/service/redis` (NIM-871); the loader and the multi-container machinery it delivered are still in use. | done, subject removed |
| **L3b-6** | ~~Drift-live~~ — removed with the drift circuit (NIM-446). It was the only live exercise of `core.file.Plan` end to end; the module's own `Plan` is still covered by its unit tests, but nothing drives it over a real Soul any more. **Errand's dry-run is the intended replacement, and since NIM-488 it reaches `Plan` at all** — the runner used to ask the `ErrandReadSafe` (Apply-path) condition on the dry-run path too, and since the two marker sets do not intersect, every `dry_run: true` Errand was terminal before reaching a `Plan`. Admission is now per-path (`dry_run: true` requires `PlanReadSafe`), so `core.file.present` plans on request while its `Apply` stays closed to ad-hoc. **It is not yet an equal replacement:** the `PlanEvent` the module sends is dropped on the floor (`errandrunner` builds a plan collector and never reads it; `ErrandResult` has no `changed` field at all — NIM-487), so a drifted host and a clean one return byte-identical results. A live test written today can assert "the host was not touched" and "Plan's body actually ran", but not "drift detected" — the verdict L3b-6 used to check. Writing that test is NIM-555 (NIM-455 asked for it first but was closed without it), and it needs NIM-487 to carry the verdict back. The inventory that used to be duplicated here now lives in one place, [`errandrunner/whitelist.go`](../../soul/internal/runtime/errandrunner/whitelist.go). | removed, superseded by NIM-555 |

## Tests

| Test | Souls | What it checks |
|---|---|---|
| `TestL3bBootstrap_OneSoul` | 1 | Real CSR Bootstrap flow + `souls.status=connected` after `SpawnSoulContainer`. |
| `TestL3bSmokeNginxLive_InstallAndStart` | 1 | Real `apt install nginx` + `systemctl start nginx` + `core.file.rendered` site config. Uses `harness.LoadExpectations` + `Stack.AssertExpectations`. |
| `TestL3bStagedProbeLive_WhereTargetsOnlyMaster` | 2+ | **staged-render probe→where on a live soul** (ADR-056): a real probe step emits a per-host register, and the Passage action `where: register.*=='master'` is genuinely applied ONLY on the master host. L3b analog of `TestE2EStagedFailover_2Passage`, but via a real apply instead of a stub. |
| `TestE2EBeaconPlugin_FullLoop` | 1 | Real `soul_beacon` plugin (gRPC-over-stdio): inotify portent → Vigil → Decree → Oracle → fired scenario on a live soul. |

## Known coverage blockers (NOT-L3b-able)

### ★ No live test drives a service day-2 any more (NIM-871)

This tier used to carry a service create and six day-2 operations against it —
`TestL3bRedisLive_Day2{AddUser,UpdateConfig,Restart,UpdateUsers,Destroy,RotateTls}`,
all in `E2E_GATE_TESTS`, all running `examples/service/redis`. That service left the
engine, and nothing in the tree replaces it: the remaining corpus services are
render-only (L0), and the three tests left in the gate prove a module is **delivered**,
not that a service is **operated**.

So the whole class is uncovered here: create against real hosts, an operational
scenario through the ADR-065 plugin channel, a state migration on a live incarnation,
CA rollover. The successor is the out-of-tree service repo's own live suite. Until it
exists, a day-2 regression reaches a tag unchallenged — this is a hole, not a
simplification, and it is the first thing to check when that repo grows a CI.

### Layer-1 finding: the form invariant "secret = vault:-ref only" is unenforceable

`pattern: "^vault:.*"` on a secret input (e.g. redis_password) **doesn't work**:
the vault: ref is resolved into a literal BEFORE value validation
(`ResolveInputValuesVault`: merge → vault-resolve → validate), so the pattern
ends up checking the already-resolved value and always fails. The pattern was removed from
`scenario/create/main.yml` and `scenario/add_replica/main.yml` (replaced with
a comment). Candidate for a separate input key `vault_ref_required` (validation
BEFORE resolve) — **out of MVP**, needs_architect.

## Expectations YAML loader (L3b-5)

`harness.LoadExpectations(t, path)` → `*Expectations` parses YAML with
`apply_runs` / `incarnation_state` / `audit_events` / `metrics` / `host_state`
(strict-mode `KnownFields(true)` — typos in keys are caught at startup).
`Stack.AssertExpectations(t, exp, applyID, incName)` applies the whole set
of checks in one call (including per-soul container-side asserts via `host_state`).

The format is fully symmetric with L3a (see [docs/testing/e2e.md](../../docs/testing/e2e.md))
plus a new `host_state` section — a list of per-soul expectations
(packages/services/files). Resolving `host_state[].soul` (by FQDN) →
`Stack.SoulContainers[i]` happens inside the loader, the caller doesn't work with indices.

## Why harness code is duplicated from L3a

Architect verdict `a0af3d90ec118aafd`: the harness package `tests/e2e/harness/`
is under Go-internal rules and unreachable from `tests/e2e-live/` (a different
module root). Duplication is acceptable because L3a/L3b are **independent
test frequencies** with different contracts (stub vs real soul). After
both stabilize — extracting a shared core into `tests/e2e-shared/` as a separate slice.

## Source of truth

- [ADR-039 § E2E three levels](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity).
- Architect consultation `a0af3d90ec118aafd` (5-slice L3b map).
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — L3a canon for fixtures/expectations
  (L3b format symmetric + container-side expectations).
