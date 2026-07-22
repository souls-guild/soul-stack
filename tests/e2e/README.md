# tests/e2e — L3a contract tier (fast-loop)

L3a-level E2E testing per [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity).

**Status:** the harness works. Every test spins up an isolated stand via
testcontainers (PG / Redis / Vault), starts a **real `keeper` process**
(a subprocess of the actual binary, not an in-process import), performs a real
`keeper init` (bootstrapping the first Archon), and opens **live gRPC-mTLS
streams** to soul-stubs against it. The Keeper<->protocol contract is verified
against live infrastructure; the only thing simulated is the apply itself on the
host: the soul-stub responds with a **scripted RunResult** instead of actually
executing tasks.

## Three E2E tiers (ADR-039)

| Tier | Directory | build-tag | Soul | What it proves |
|---|---|---|---|---|
| **L3a** contract | `tests/e2e/` | `e2e` | soul-**stub** (scripted RunResult) | Keeper<->protocol contract: apply_runs lifecycle, dispatch routing, staged-render (probe→where), state-commit, audit/metrics — on live PG/Redis/Vault/keeper, but without a real apply on the host. |
| **L3b** live | `tests/e2e-live/` | `e2e_live` | a **real** `soul` binary in a privileged Debian-systemd container | a real CSR handshake + a real apply on the host (apt install, systemctl, files) — see [tests/e2e-live/README.md](../e2e-live/README.md). |
| **L3c** k8s | `tests/e2e-k8s/` | `e2e_k8s` | a real `soul` in a kind cluster | production-like deployment (weekly/pre-release). |

L3a answers the question "does Keeper correctly render Destiny, route dispatch over
live streams, aggregate RunResult, commit state and write audit/metrics." L3b answers
the question "does the real module actually change the host correctly." Splitting by
contract gives a fast-loop (L3a — seconds to minutes, every PR) versus a smoke-loop
(L3b — minutes, nightly).

## Running

```sh
make e2e
# equivalent to:
cd tests/e2e && go test -tags=e2e -timeout=10m ./...
```

Pre-flight in `harness.NewStack`:

- **keeper binary** is required. Source — env `KEEPER_BIN`, otherwise the default
  build `keeper/bin/keeper` (`make build`). Without the binary the test Skips BEFORE
  spawning testcontainers (so a developer without a build doesn't wait through a
  5-minute timeout).
- **docker** is required for testcontainers. If docker is present but spawning
  PG/Redis/Vault fails — the test fails explicitly (the developer requested E2E
  intentionally, absence of docker = fail, not skip).

Building and `go vet -tags=e2e ./...` always pass (a separate go module
`tests/e2e/go.mod` — testcontainers deps don't leak into `keeper/`/`soul/`).

## Harness architecture

`harness.Stack` is the unit of isolation: one test = one Stack = its own PG/Redis/Vault +
its own Keeper process + N soul-stubs. `NewStack` blocks until fully ready
(PG healthy → Vault test-secrets → keeper init → keeper run responds to `/readyz`
→ soul-stubs pre-auth-registered).

What `NewStack` does (`harness/stack.go`):

1. `startPostgres` / `startRedis` / `startVault` — testcontainers (`postgres:16-alpine`,
   `redis:7-alpine`, `hashicorp/vault:1.15` dev-mode).
2. `InitVaultTestSecrets` — sets up a PKI mount + JWT signing-key in Vault (symmetric
   with prod provisioning). `IssueKeeperServerCert` — issues the server cert for
   Keeper listeners.
3. `buildKeeperYAML` — renders `keeper.yml` into tmpDir with resolved infra endpoints.
4. `runKeeperInit` — `keeper init --archon=archon-test --credential-out=...`; the JWT
   of the first Archon is read from the credential file into `Stack.JWT`.
5. `startKeeperRun` — `keeper run` as a subprocess; stdout/stderr are forwarded to
   `t.Log`; polls `/readyz` (60s).
6. `RegisterSoulPreAuth` for each `Config.Souls` — inserts the SID into the `souls`
   registry and issues an mTLS client cert (needed to later open a stream).

Architectural invariants (ADR-039 Amendment 2026-05-26):

- the harness does NOT import `keeper/internal/*` (Go internal rules);
- all DB operations go through direct SQL via `pgx`;
- all Vault operations go through direct HTTP API (`harness/vault.go`);
- the Keeper process is a subprocess of the real binary.

### soul-stub

`Stack.ConnectSoulStub(t, i)` (`harness/soul_stub.go`) opens a **live
EventStream gRPC-mTLS stream** for the i-th pre-auth Soul to Keeper. This turns
"a row in `souls` with `status=connected`" into a real stream: on session-open, Keeper
grabs a Redis SID-lease, and dispatch (Apply/Errand) is routed to that SID's local
Outbound. Without an open stream, dispatch returns `ErrSoulNotConnected`
(apply → `orphaned`). `ConnectSoulStub` waits for `HelloReply` — a guarantee that the
lease has been grabbed and dispatch will be routed.

The stub (`internal/soulstub/`) responds to `ApplyRequest` with a scripted
`RunResult`:

- `LoadApplyScript` / `SetApplyDefaultSuccess` — success responses per task / by
  default;
- `SetTaskRegister(taskName, data)` — per-host `register` data (`TaskEvent.RegisterData`,
  echo pass-through) for staged-render probe→where;
- `SetErrandStatus` — the ErrandResult branch;
- `Messages()` — recorded FromKeeper frames (for assertions: which ApplyRequest and
  with which `passage` arrived).

### Stack helpers

Drivers (`harness/stack.go`):

- `CreateIncarnation` / `CreateIncarnationWithApply` / `CreateIncarnationRaw` —
  POST `/v1/incarnations` via Operator API (with polling for the transient 422
  "service not registered" while serviceregistry.Holder warms up its snapshot).
- `RunScenario(inc, scenario, input)` — POST a scenario scan, returns `apply_id`.
- `SeedIncarnationReady` — directly INSERTs a ready incarnation with a baseline
  state (for mutating scenarios whose `create` is unavailable in L3a — cloud-spawn /
  declared-role / probe on a not-yet-started host, e.g. redis-cluster).
- `WaitApplySuccess` — blocks until `apply_runs.status=success` for all rows of the run.
- `WaitIncarnationReady` — blocks until `incarnation.status=ready` (state_changes are
  committed AFTER the barrier across all hosts; you must wait for `ready`, not just
  success).

Fixture helpers: `RegisterService`, `MaterializeDestinies`, `SeedSoulprint`,
`AddMember` (roster via `incarnation_membership`, ADR-008 amendment/NIM-124),
`SeedVaultKV`.

Assertions (`harness/asserts.go`):

- `AssertApplyRunsStatus(applyID, expected)`;
- `AssertIncarnationState(name, expectedSubset)` — subset comparison of `incarnation.state`;
- `AssertAuditEvent(eventType, expectedPayload)`;
- `AssertMetricGE(metric, minimum)` — keeper's Prometheus endpoint.

**Key invariant:** status-field values in assertions must be the real
enum values from the Go code only (`apply_runs.status: success`, not
`succeeded`/`done`). `TestValidApplyRunsStatus_*` guarantees that the list of valid
values hasn't drifted from the Go enum.

## Layout

```
tests/e2e/
├── README.md                  # this file
├── go.mod / go.sum            # separate go module (testcontainers deps isolated)
├── harness/                   # the working L3a harness
│   ├── stack.go               # NewStack / Cleanup / drivers (Create/Run/Wait*)
│   ├── soul_stub.go           # ConnectSoulStub + LoadApplyScript
│   ├── asserts.go             # AssertApplyRunsStatus / IncarnationState / AuditEvent / MetricGE
│   ├── vault.go               # InitVaultTestSecrets / IssueKeeperServerCert / SeedVaultKV
│   ├── cert.go                # RegisterSoulPreAuth + mTLS client-cert
│   ├── config_builder.go      # buildKeeperYAML
│   ├── git.go / destiny.go    # bare git repo per service + MaterializeDestinies
│   ├── fixtures.go            # YAML loader for souls/soulprint
│   ├── drift.go / errand.go   # drift-check + errand driver
│   ├── oracle.go / probe.go   # Vigil/Oracle event-driven helpers
│   └── operator.go            # HTTP client for the Operator API with JWT
├── internal/
│   └── soulstub/              # fake-Soul helper package (NOT a binary, ADR-004)
├── smoke-nginx/  hello-world/  noop/  coven-probe/  long-runner/
│   └── fixtures/ + expectations/   # per-example service fixtures
├── redis/  redis-cluster-live/  redis-monitored/  redis-sentinel/
├── staged-failover/           # WIP service for the staged-render proof (NOT examples/**)
├── oracle_typed_portent/
└── <name>_test.go             # one file per case
```

## What's covered (L3a tests)

| Test | Souls | What it proves |
|---|---|---|
| `TestSmokeNginx_InstallAndStart` | 1 | pilot: `create` smoke-nginx, apply_runs lifecycle + state-commit. |
| `TestE2EServiceHelloWorld_Create` | 1 | required `input.greeting` + `state.greeting_file` mutation. |
| `TestE2EServiceNoop_Create` | 1 | minimal no-op `core.exec.run`, state not mutated. |
| `TestE2EServiceCovenProbe_Create` | 1 | `core.file.present` init marker + double state mutation. |
| `TestE2EServiceLongRunner_Create` | 1 | `core.file.present` + double state mutation. |
| `TestE2EStagedFailover_2Passage` | 2 | **staged-render probe→where** (ADR-056): Passage 0 probes everyone, Passage 1 `where: register.role=='master'` ONLY on the master. |
| `TestE2EServiceRedisCluster_UpdateAcl` | 3 | redis-cluster `update_acl` on a PG-seeded ready incarnation: per-host role probe (3 hosts) + ACL-apply only-master + state.redis_users patch + scoped-vault reachability. |
| `TestE2EServiceRedisClusterLive_*` (`Create`/`AddUser`/`UpdateConfig`/`UpdateAcl`/`AddReplica`/`RemoveReplica`/`Reshard`) | 1–3 | redis-cluster-live on core modules: lifecycle of mutating scenarios. |
| `TestE2EServiceRedis_*` (`Create`/`AddAclUser`/`UpdateConfig`/`AddReplicas`/`*NodeExporter`) | 1+ | redis service + node-exporter destiny. |
| `TestE2EServiceRedisMonitored_Create` / `TestE2EServiceRedisSentinel_Create` | | redis + monitoring / sentinel topology. |
| `TestE2EKeeperSideDispatch_CovenRegistered` | | keeper-side core `core.soul.registered` (`on: keeper` dispatcher). |
| `TestE2EOracleTypedPortent_*` / `TestOracle_FileChanged_FiresScenario` / `TestL3b_VigilDecreeOracleFlow_Smoke` | | Vigil/Oracle event-driven: portent → fired scenario. |
| `TestDrift_CheckDrift_DriftedAndClean` | 1 | drift-check (stub-Plan; the real Plan is in L3b). |
| `TestSoulHistory_AggregatesScenarioAndErrand` | | scenario+errand history aggregation by SID. |
| `TestIncarnationCreate_MissingRequiredInput_422` | | negative: sync validation of required input. |
| `TestValidApplyRunsStatus_*` | — | guard: `apply_runs.status` enum values haven't drifted from Go. |

## How to add an L3a case

1. Place the service fixture in [`examples/service/<name>/`](../../examples/service/)
   (regular service format — the harness reads it "as is" via the git-loader).
   A WIP service for proving a specific mechanism goes next to it in
   `tests/e2e/<name>/` (like `staged-failover/`), to keep examples clean.
2. Create `tests/e2e/<name>_test.go` with the `//go:build e2e` tag:
   ```go
   func TestE2EService<Name>_Create(t *testing.T) {
       stack := harness.NewStack(t, harness.Config{
           ExamplePath: "examples/service/<name>",
           Souls:       1,
       })
       defer stack.Cleanup()

       stack.RegisterService(t, "<name>", "examples/service/<name>")
       stub := stack.ConnectSoulStub(t, 0)
       stub.SetApplyDefaultSuccess(true)

       inc, applyID := stack.CreateIncarnationWithApply(t, "<inc>", "<name>@main", input)
       stack.WaitApplySuccess(t, applyID, 60)
       stack.WaitIncarnationReady(t, inc, 30)
       stack.AssertIncarnationState(t, inc, expected)
   }
   ```
3. Run `make e2e`.

## Source of truth

- [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity) — the three E2E levels.
- [docs/testing/README.md](../../docs/testing/README.md) — index of L0/L1/L2/L3a/L3b/L3c.
- [docs/testing/e2e.md](../../docs/testing/e2e.md) — normative spec of the fixtures/expectations format.
- [tests/e2e-live/README.md](../e2e-live/README.md) — the L3b live tier.
