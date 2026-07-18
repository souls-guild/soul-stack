# tests/e2e-live — L3b real-soul-in-container (ADR-039)

An L3b smoke loop with a **real** `soul` binary in a Linux container (Debian-12
systemd-PID-1). Unlike L3a (`tests/e2e/`, a soul-stub helper package), L3b:

- uses a real `soul` binary inside a privileged container with systemd-PID-1;
- goes through the real CSR Bootstrap flow (CSR → `Keeper.Bootstrap` → leaf cert);
- a real `core.pkg.installed name=nginx` installs a real package;
- a real `core.service.running name=nginx` starts a real service through systemd.

## Running

```sh
# Pre-requisite — cross-compile the linux binary to mount into the container
make build-linux

# Full L3b run
make e2e-live

# Or a single test directly
SOUL_BIN_LINUX=$(pwd)/soul/bin/soul-linux-amd64 \
  go test -tags=e2e_live -run TestSmokeNginxLive ./...
```

Requires **docker** with support for privileged containers (cgroup-mount + systemd-PID-1).

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

## Frequency

- **L3a** (fast loop) — every PR via `make e2e` (~30-60 sec per test).
- **L3b** (smoke loop) — **nightly cron / on-demand** (~5-15 min per test).
- **L3c** (k8s loop) — weekly / pre-release.

## Layout

```
tests/e2e-live/
├── README.md
├── go.mod                          # separate go module (deps don't leak)
├── dockerfiles/
│   └── debian-12.Dockerfile        # privileged systemd-PID-1 base image
├── harness/                        # copy of L3a harness + L3b-specific helpers
│   ├── stack.go                    # NewStack — PG/Redis/Vault/keeper
│   ├── bootstrap.go                # IssueBootstrapToken
│   ├── container.go                # SoulContainer + SpawnSoulContainer (privileged Debian-12)
│   ├── asserts.go                  # keeper-side + container-side asserts (AssertHost*)
│   ├── expectations.go             # LoadExpectations / AssertExpectations (+ host_state)
│   ├── coven.go                    # AddMember (roster via incarnation_membership)
│   ├── operator.go                 # Operator API HTTP client
│   ├── vault.go                    # InitVaultTestSecrets / IssueKeeperServerCert / SeedVaultKV
│   ├── config_builder.go           # buildKeeperYAML
│   ├── probe.go                    # waitForReady
│   ├── git.go                      # SetupGitRepo
│   ├── destiny.go                  # MaterializeDestinies
│   └── drift.go                    # CheckDrift → DriftReport (real core.file.Plan)
├── smoke-nginx-live/  redis/  redis-cluster-live/  staged-probe-live/
│   └── …/expectations/             # per-example host_state expectations
├── smoke_bootstrap_test.go         # TestL3bBootstrap_OneSoul
├── smoke_nginx_live_test.go        # TestL3bSmokeNginxLive_InstallAndStart
├── redis_live_test.go              # TestL3bRedisLive_CreateWithNodeExporter
├── redis_cluster_live_test.go      # TestL3bRedisClusterLive_ThreeNode
├── staged_probe_live_test.go       # TestL3bStagedProbeLive_WhereTargetsOnlyMaster
├── drift_live_test.go              # TestL3bDriftLive_HelloWorld
└── plugin_beacon_test.go           # TestE2EBeaconPlugin_FullLoop
```

## Slices

L3b is implemented iteratively. Slice map (architect consultation `a0af3d90ec118aafd`):

| Slice | Content | Status |
|---|---|---|
| **L3b-1** | Layout + Dockerfile + `make build-linux` + `harness.NewStack` skeleton (PG/Redis/Vault/keeper, WITHOUT soul-container). | done |
| **L3b-2** | Real Bootstrap flow: `IssueBootstrapToken` + `SpawnSoulContainer` (privileged Debian-12 + soul-binary mount + CSR Bootstrap RPC). | done |
| **L3b-3** | First L3b example `smoke-nginx-live` (actually installs nginx via apt + systemctl start). | done |
| **L3b-4** | Container-side asserts (`AssertHostPkgInstalled` / `AssertHostServiceActive` / `AssertHostFileExists` / `AssertHostFileContent`). | done |
| **L3b-5** | Multi-host (`redis-cluster-live` with 3 soul containers) + YAML expectations loader (`harness.LoadExpectations` / `Stack.AssertExpectations`). | done |
| **L3b-6** | Drift-live (`drift_live_test.go` + `harness/drift.go`): check-drift on a live soul through a real `core.file.Plan` (`core.file.present` module), not a stub Plan as in L3a. | done |

## Tests

| Test | Souls | What it checks |
|---|---|---|
| `TestL3bBootstrap_OneSoul` | 1 | Real CSR Bootstrap flow + `souls.status=connected` after `SpawnSoulContainer`. |
| `TestL3bSmokeNginxLive_InstallAndStart` | 1 | Real `apt install nginx` + `systemctl start nginx` + `core.file.rendered` site config. Uses `harness.LoadExpectations` + `Stack.AssertExpectations`. |
| `TestL3bRedisLive_CreateWithNodeExporter` | 1 | Real redis service: `apt install redis` + node-exporter destiny on a live host. |
| `TestL3bRedisClusterLive_ThreeNode` | 3 | Multi-host: install redis on 3 containers + form a Redis Cluster (`redis-cli --cluster create --cluster-replicas 0`); independent check `cluster_state:ok`. |
| `TestL3bStagedProbeLive_WhereTargetsOnlyMaster` | 2+ | **staged-render probe→where on a live soul** (ADR-056): a real probe step emits a per-host register, and the Passage action `where: register.*=='master'` is genuinely applied ONLY on the master host. L3b analog of `TestE2EStagedFailover_2Passage`, but via a real apply instead of a stub. |
| `TestL3bDriftLive_HelloWorld` | 1 | Drift check on a live soul: create hello-world (`core.file.present` greeting file) → clean baseline → out-of-band file mutation → `CheckDrift` sees `drifted=1` via a real `core.file.Plan` → re-apply → `CheckDrift` clean again. Catches real Plan regressions (unlike L3a's stub Plan). |
| `TestE2EBeaconPlugin_FullLoop` | 1 | Real `soul_beacon` plugin (gRPC-over-stdio): inotify portent → Vigil → Decree → Oracle → fired scenario on a live soul. |
| `TestL3bRedisClusterCreate_FullLifecycle` | 3 | **SKIPPED (structural blocker, see below)**. Body kept: documents the target create-lifecycle + a reusable harness `SeedIncarnationForCreate` (direct-SQL seed of a declared role's spec.hosts[]). |

## Known coverage blockers (NOT-L3b-able)

### `redis-cluster` (full) create — host-variant destiny-when (layer 3)

`TestL3bRedisClusterCreate_FullLifecycle` is marked `t.Skip` and **not counted** in
coverage. The reason is a structural defect in the service itself, not the test:

- destiny `redis-replication-config` (`tasks/main.yml`) uses
  host-variant flow control `when: soulprint.self.<...> != input.master_addr`
  on a multi-host `apply: destiny:`. The engine rejects this
  (`guardFlowControlHostInvariant`, split-brain prevention; per-host
  destiny-dispatch is a deferred engine feature, a separate ADR).
- The canon (ADR-009 / [orchestration §4.1](../../docs/scenario/orchestration.md))
  intends a **host-invariant destiny** + per-role targeting at the scenario level
  (`where:`). redis-cluster was written with the wrong pattern.
- The create-lifecycle **is covered** by the simplified `redis-cluster-LIVE`
  (`TestL3bRedisClusterLive_ThreeNode`): install redis on 3 nodes + form the
  cluster, independent `cluster_state:ok`.
- Reactivation once: **(a)** redis-cluster create is rewritten to per-role
  scenario steps (`where: primary`/`replica` + host-invariant destinies) —
  a service-design follow-up; **OR (b)** per-host destiny-dispatch (an engine
  feature, deferred ADR).

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
