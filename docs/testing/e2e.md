# L3a E2E - harness and fixtures/expectations format

Regulatory spec L3a fast-loop E2E testing ([ADR-039](../adr/0039-e2e-testing.md)).
Source of truth for the implementation - directory [`tests/e2e/`](../../tests/e2e/) (Go code)
and this document (YAML-fixtures and expectations format + harness method contract).

## What is this

L3a raises an isolated E2E test bench for one test: PG + Redis + Vault via
testcontainers + Keeper-process (in-process) + N soul-stubs (helper-package,
not binary). The test runs the scenario via the Operator API, soul-stub responds
scripted `RunResult`-ohm, test assertit `apply_runs` / `incarnation.state` /
audit / metrics.

**L3a DOES NOT test** real apply on the host (L3b real-soul-in-container is for that).
L3a - keeper-side test contract (scenario-runner ↔ apply_runs lifecycle ↔
audit ↔ Prometheus).

## Launch

```sh
make e2e
# or directly:
cd tests/e2e && go test -tags=e2e -timeout=10m ./...
```

Without docker, tests are skipped (testcontainers spawn returns an error). Assembly
(`go build -tags=e2e ./tests/e2e/...`) and vet (`go vet -tags=e2e ./tests/e2e/...`)
always work - this is an invariant of ADR-039: pilot must remain
compile-clean even without docker.

## Layout

```
tests/e2e/
├── go.mod                          # separate gorod module (deps do not leak)
├── README.md
├── harness/                        # reusable Go-helpers
├── internal/soulstub/              # fake-Soul helper-package
├── <example-slug>/                 # fixtures + specific test expectations
│   ├── fixtures/
│   │   ├── souls.yaml              # SID + corresponding soulprint + status
│   │   └── stub-responses.yaml     # scripted RunResults of soul-stub
│   └── expectations/
│       └── after-<scenario>.yaml   # apply_runs / state / audit / metrics
└── <example-slug>_test.go          # Go-test with //go:build e2e
```

Service-fixture (what Keeper downloads via git) is **in `examples/service/<name>/`**,
and not in `tests/e2e/<name>/`. This is an invariant of ADR-039(5): "examples remain
domain-pure; the test harness lives in `tests/e2e/<name>/`".

## fixtures format

### `fixtures/souls.yaml`

List of soul-stubs that the harness registers with the Keeper before running.

```yaml
- sid: soul-a.example.com
  status: connected                  # value souls.status; usually "connected"
  covens: [smoke]                    # Coven membership for where:-targeting
  soulprint:                         # contents of soulprint_facts (see docs/soul/soulprint.md)
    os:
      family: debian
      distro: debian
      version: "12"
      pkg_mgr: apt
      init_system: systemd
    hostname: soul-a
```

| Key | Type | Obligation | Description |
|---|---|---|---|
| `sid` | string | yes | Soul's FQDN name ([naming-rules.md → SID](../naming-rules.md)). |
| `status` | string | yes | Target `souls.status` (usually `connected`). |
| `covens` | []string | no | Coven labels for where: filters; empty = incarnation-membership only. |
| `soulprint` | map | no | Contents `soulprint_facts` (form - typed-schema SoulprintFacts, see [docs/soul/soulprint.md](../soul/soulprint.md)). |

### `fixtures/stub-responses.yaml`

Soul-stub response script: responds to each task from the scenario stub
corresponding to `RunResult`.

```yaml
scenarios:
  <scenario-name>:
    apply_responses:
      - task_name: "Install nginx package"
        run_result:
          status: success
          state_changes:
            packages:
              - nginx: installed
```

| Key | Type | Obligation | Description |
|---|---|---|---|
| `scenarios` | map | yes | Map scenario-name → script. The keys match `name:` in `scenario/<name>/main.yml`. |
| `<scenario>.apply_responses[]` | list | yes | List of scripted responses. Matching by `task_name` (not by index - order may vary). |
| `apply_responses[].task_name` | string | yes | Complete `name:` tasks from destiny/scenario. |
| `apply_responses[].run_result.status` | string | yes | The actual value of enum is `keeper/internal/applyrun.Status` (for example `success`, `failed`). Harness validates fail-early. |
| `apply_responses[].run_result.state_changes` | map | no | An arbitrary jsonb-payload, placed in the `state_changes` field `FromSoul.RunResult`. |

### `expectations/after-<scenario>.yaml`

Post-apply waiting. Harness reads db/audit/metrics after `WaitApplySuccess`
and compares with this file.

```yaml
apply_runs:
  status: success                            # real enum value

incarnation_state:                            # deep-subset jsonb check
  nginx_package: nginx
  nginx_service: nginx

audit_events:                                 # presence check (minimum one line)
  - type: incarnation.created                 # real EventType (shared/audit/event_types.go)
    payload:
      incarnation: test-nginx

metrics:                                      # Prometheus-expression → constraint-string
  keeper_scenario_runs_total{result="ok"}: ">= 1"
```

> The names of audit events and metrics in expectations are **real values from Go code**, not made up. The source of truth for audit types is [`shared/audit/event_types.go`](../../shared/audit/event_types.go) (for example, `incarnation.created`, `incarnation.scenario_started`, `incarnation.run_completed`; there are NO events `scenario.applied` / `scenario.completed` in the code). The source for run metrics is [`keeper/internal/scenario/metrics.go`](../../keeper/internal/scenario/metrics.go): the metric is called `keeper_scenario_runs_total{result="ok"|"error"|"locked"}` (there are NO metrics `keeper_apply_runs_total` in the code).

| Key | Semantics |
|---|---|
| `apply_runs.status` | The equal value of `apply_runs.status` at the time of the terminal. Only real enum-values: `planned`, `claimed`, `running`, `dispatched`, `success`, `failed`, `cancelled`, `orphaned`, `no_match`. |
| `incarnation_state` | Deep-subset check `incarnation.state` jsonb. The specified keys must be equal (for scalars) / contain a subset (for maps). Extra keys in the database are allowed. |
| `audit_events[]` | Presence check: for each line in expectation in `audit_log` there must be at least one line with `type=<type>` and `payload` containing `<payload>` subset. Cardinality is not checked. |
| `metrics{<expression>}` | Prometheus-expression → constraint-string (`>= N`, `== N`, `> N`). Harness scrapes `/metrics` Keeper, parses it and applies constraint. |

## Source-of-truth for enum values

[ADR-039(4)](../adr/0039-e2e-testing.md):
status values in expectations are **real enum-values** from Go code
keeper.

Harness validates `apply_runs.status` at the start of the test via
`harness.CheckApplyRunsStatusValid`. List of allowed values ​​- in
[`tests/e2e/harness/asserts.go`](../../tests/e2e/harness/asserts.go) (map
`validApplyRunsStatus`). Source - [`keeper/internal/applyrun/applyrun.go`](../../keeper/internal/applyrun/applyrun.go)
(consts `StatusSuccess` etc.); drift is caught by a test that checks the map against the Go-enum.

## Contract harness

```go
import "github.com/souls-guild/soul-stack/tests/e2e/harness"

func TestX(t *testing.T) {
    stack := harness.NewStack(t, harness.Config{
        ExamplePath: "examples/service/<name>",
        Souls:       1,
    })
    defer stack.Cleanup()

    inc := stack.CreateIncarnation(t, "test-x", "<service>@main", map[string]any{...})
    applyID := stack.RunScenario(t, inc, "<scenario>", map[string]any{...})

    stack.WaitApplySuccess(t, applyID, 60)

    stack.AssertApplyRunsStatus(t, applyID, "success")
    stack.AssertIncarnationState(t, inc, map[string]any{...})
    stack.AssertAuditEvent(t, "incarnation.created", map[string]any{...})
    stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}
```

| Method | Contract |
|---|---|
| `NewStack(t, Config) *Stack` | Raises PG/Redis/Vault via testcontainers + Keeper process + N soul stubs. Blocked until `keeper /readyz=200` and registration of all Souls. |
| `Stack.Cleanup()` | Graceful-shutdown of the entire stand + removal of tmp directories. Idempotent. |
| `Stack.CreateIncarnation(t, name, serviceRef, spec)` | Creates an incarnation via the `POST /v1/incarnations` Operator API. Returns the name. |
| `Stack.RunScenario(t, inc, scenario, input)` | Runs scenario via `POST /v1/incarnations/<name>/scenarios/<scenario>`. Returns apply_id. |
| `Stack.WaitApplySuccess(t, applyID, timeoutSec)` | Polling `apply_runs.status` to terminal or timeout. The test fails if the status is != success. |
| `Stack.AssertApplyRunsStatus(...)` | Direct SELECT on apply_runs with expected status validation via `validApplyRunsStatus`. |
| `Stack.AssertIncarnationState(...)` | SELECT `incarnation.state` jsonb + deep-subset comparison. |
| `Stack.AssertAuditEvent(...)` | SELECT `audit_log` WHERE type=... + payload subset check. |
| `Stack.AssertMetricGE(...)` | GET `<keeper>/metrics`, parsing Prometheus-text, eval expression from the key. |

## Soul-stub contract

See [`tests/e2e/internal/soulstub/`](../../tests/e2e/internal/soulstub/).

- `New(sid, keeperGRPCAddr) *Stub` - constructor.
- `Stub.LoadScript(perScenario)` — loading scripted responses from parsed `stub-responses.yaml`.
- `Stub.Open()` — open bidi-stream to Keeper (mTLS-handshake, SID registration), blocked until Close or break.
- `Stub.Close()` — graceful-shutdown.
- `Stub.Messages() []Message` — all payloads received from Keeper for subsequent assertions.

## L3a canonical config & helpers

Specific canonical parameters on which harness and smoke-test are raised
isolated stand (see also [ADR-039 Amendment 2026-05-26](../adr/0039-e2e-testing.md)).

| Parameter | Canon | Source of truth |
|---|---|---|
| Spawn infra | testcontainers-go (PG/Redis/Vault); Keeper is a sub-process of the real `keeper` binary | ADR-039 Amendment §1 |
| Vault KV path for JWT | `secret/keeper/jwt-signing-key`, field `signing_key` | `dev/keeper.dev.yml::auth.jwt.signing_key_ref` |
| Vault KV format signing-key | **HS256: 32B random base64-encoded string** (NOT Ed25519 PEM) | `keeper/internal/jwt/issuer.go::minSigningKeyBytes`, `dev/provision.sh:88-92` |
| Vault PKI mount | `pki/`, role `soul-seed`, root CA TTL 87600h | `dev/provision.sh` steps 3-5 |
| Keeper TLS cert | Vault-issued leaf, SAN `127.0.0.1`+`localhost`, TTL 24h | `harness.IssueKeeperServerCert` |
| Sigil signing-key | NOT pre-seed; `KeyService` introduces runtime | `keeper/internal/sigil/keyservice.go` |
| Bootstrap of the first Archon | `keeper init --archon=archon-test --config=<path> --credential-out=<tmp>/jwt` | `keeper/cmd/keeper/main.go:99-261` |
| Env keeper run | `SOUL_STACK_ALLOW_FILE_REPOS=1` is required for service-loader with file://-URL | ADR-039 Amendment §5 |
| Soul-stub pre-auth | Vault PKI leaf + direct SQL INSERT into `souls`/`soul_seeds`; `bootstrap.Bootstrap` passes | ADR-039 Amendment §6 |
| harness ↔ keeper boundaries | harness does NOT import `keeper/internal/*` (Go internal-rules); all DB-ops - direct SQL, Vault-ops - direct HTTP API | ADR-039 Amendment §1 |

**Drift-invariant.** Drift between the harness CRUD framework and the real one
schema-set in `keeper/migrations/` is manually synced when schema-change occurs.
The minimum set of affected tables for harness is `souls`, `soul_seeds`,
`operators`, `apply_runs`, `incarnation`, `audit_log`. Source of Truth - Go-
structures and migrations to `keeper/internal/`; harness-CRUD is duplicated by
necessary (via replace in `go.mod` is not carried now - internal-rules
prohibit import from the test module).

**Note about HS256 vs Ed25519.** Before L3a-impl v3, it was mistakenly included in my technical specification
Ed25519 PEM as a signing-key format - this contradicts `jwt/issuer.go`. Real
format - HMAC-256 key ≥32 bytes; `bootstrap.extractSigningKey` tries first
base64-decode, on error accepts the string as raw-bytes (see `keeper/internal/
bootstrap/keeper_init.go::extractSigningKey`). The harness writes `signing_key` as a
base64-encoded string of 32 random bytes (`crypto/rand` → `base64.StdEncoding`),
is completely symmetrical to `openssl rand -base64 32` in `dev/provision.sh:90`.

## L3a implementation status

L3a-harness **fully implemented** (phase v3, pilot-stub removed): `NewStack`
makes real infrastructure spawn through testcontainers (PG / Redis / Vault), raises
real `keeper` sub-process, opens a real gRPC stream from
soul-stub, loads fixtures/expectations with YAML loader and executes real ones
`Assert*` methods.

`t.Skip` remains in exactly two cases meaning "E2E is not possible in this environment"
and not "not implemented":

- no keeper binary (`KEEPER_BIN` not set and `make build` not executed) - skip BEFORE
spawn so that the developer without the build does not catch a 5-minute timeout;
- no docker - testcontainers will return a spawn error; in this case the test **fails
explicitly** (E2E was requested deliberately, lack of docker - fail, not skip).

Assembly/vet with `-tags=e2e` must always pass (invariant ADR-039 - compile-clean
without docker).

## L3b (real-soul-in-container) — `tests/e2e-live/`

Status: **L3b-1..L3b-5 are closed** (all 5-slice map L3b is completed).
Smoke tests:

- `TestL3bBootstrap_OneSoul` - real CSR Bootstrap-flow + `souls.status=connected`.
- `TestL3bSmokeNginxLive_InstallAndStart` - real `apt install nginx` +
  `systemctl start` + `core.file.rendered` (single-host).
- `TestL3bRedisClusterLive_ThreeNode` - multi-host (3 soul containers) +
Redis Cluster via `redis-cli --cluster create`.

Build-tag: `e2e_live`. Run: `make e2e-live` (requires `make build-linux` +
docker with privileged mode). Frequency: on-demand (pre-tag gate in
[RELEASING.md](../../RELEASING.md); daily nightly-cron is disabled - manual-only).

**WSL2.** On WSL2, the container does not reach the keeper via
`host.docker.internal` — pass the real host IP through `E2E_KEEPER_HOST`
(harness will write it to the `soul.yml` endpoint). Recipe:

```sh
E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') make e2e-live
```

Without `E2E_KEEPER_HOST` the behavior does not change (CI default `host.docker.internal`).
Full quickstart - [`tests/e2e-live/README.md`](../../tests/e2e-live/README.md).

Differences from L3a:

- Instead of the soul-stub helper package - a real `soul`-binary in privileged Debian-12
container with systemd-PID-1 (see `tests/e2e-live/dockerfiles/debian-12.Dockerfile`).
- Real CSR Bootstrap-flow (CSR → `Keeper.Bootstrap` RPC → leaf-cert) instead
pre-auth registration in the database.
- Real `core.pkg.installed` / `core.service.running` mutate state
inside a container (apt-install + systemctl).

### YAML expectations loader (L3b-5)

`tests/e2e-live/harness/expectations.go` exports
`LoadExpectations(t, path) *Expectations` and `Stack.AssertExpectations(t, exp,
applyID, incName)`. The YAML format is an extension of L3a-`ExpectationsAfter`
(see above) plus section `host_state[]` for per-soul container-side assertions:

```yaml
host_state:
  - soul: soul-live-a.example.com     # FQDN, must match Stack.SoulContainers[i].SID
    packages:
      nginx: installed                # → AssertHostPkgInstalled
    services:
      nginx: active                   # → AssertHostServiceActive
    files:
      - path: /etc/nginx/...          # → AssertHostFileExists
        contains: "server_name"       # → AssertHostFileContent (optional)
```

Loader works in strict-mode (`yaml.Decoder.KnownFields(true)`) - extra
keys (typos) are caught at the start of the test, not at the assert phase. `metrics`-section
supports constraint expressions `>= N` / `<= N` / `> N` / `< N` / `== N`
or a bare number (interpreted as `>= N`); MVP only implements `>=`,
other operators → fail with a clear message (expansion without breaking
change YAML format).

L3b slice map and details - `tests/e2e-live/README.md`.

## L3c (kind-cluster, real K8s) — `tests/e2e-k8s/`

L3c - slowest E2E layer: on top of a **real** Kubernetes cluster
(kind) with infrastructure (PG / Redis / Vault) via bitnami Helm charts and Keeper / Soul
as separate k8s resources (Deployment / StatefulSet). Covers HA cases,
not available in L3a/L3b: leader-failover when killing keeper pod, Watchman-
rebalance with scale Soul-replicas, Toll-rate-limit under load.

Build-tag: `e2e_k8s`. Run: `make e2e-k8s` (requires docker + kind CLI in PATH).
Frequency: **weekly / pre-release**.

### Architecture

- kind-cluster per-test (`soul-stack-e2e-<test>-<unix-nanos>`) - isolates
parallel runs.
- Infra - bitnami Helm charts (`bitnami/postgresql` / `bitnami/redis` /
`bitnami/vault`), the repo is connected runtime via `helm repo add` with retry
(PM-decision: vendoring of charts in the repo is postponed).
- Keeper-image: base `gcr.io/distroless/static` (PM-decision).
- Soul-pod: privileged systemd-PID-1 (PM-decision: parity with L3b
  `tests/e2e-live/dockerfiles/debian-12.Dockerfile`).
- Harness code **duplicated** with L3a/L3b (Go-internal-rules; extraction
in `tests/e2e-shared/` is a separate slice after L3c-1..L3c-5).

### L3c slice map

| Slice | Contents | Status |
|---|---|---|
| **L3c-1** | Layout + Dockerfile-placeholder + `make e2e-k8s` + `harness.NewCluster` (kind via Go-API) + `TestL3cKindUp_Empty`. | done |
| **L3c-2** | `Stack.DeployInfra` (bitnami Helm PG/Redis/Vault) + `Stack.DeployKeeper` (single keeper-pod, distroless, `--initialize` mode) + Vault PKI/JWT/DSN seed via port-forward + `TestL3cKeeperPing_Single` (port-forward keeper:8080 → /readyz=200). | done |
| **L3c-3** | `Stack.DeployKeeper` replicas: 3 (HA) + `Stack.DeploySoul` (StatefulSet privileged systemd-PID-1) + `TestL3cMultiKeeper_BootstrapAndConnect`. | done |
| **L3c-4** | HA-failover scenario: `TestL3cMultiKeeper_KillLeader` (per-pod KID via init-container + reaper.enabled with lock_ttl=15s + kubectl delete leader pod → new leader in `GET reaper:leader` → Deployment self-heal → Soul reconnect). | done |
| **L3c-5** | Toll degraded-mode test (`TestL3cToll_DegradedMode`: 5 souls → kill 3 grace=0 → `cluster:degraded` flag → POST scenario=503 + Retry-After + audit `cluster.degraded_set`) + Redis-cluster resharding (parity with mega-test 2026-05-25; framework `TestL3cRedisCluster_Resharding` with t.Skip to in-cluster git-server-pod infra - propose-and-wait named `git-server-pod`). | done (part A) / deferred (part B) |

L3c - full map and quickstart - [`tests/e2e-k8s/README.md`](../../tests/e2e-k8s/README.md).

## Examples-as-tests (L3a coverage extension)

L3a-pilot `smoke-nginx` fixed pattern; then the pattern is replicated to
existing `examples/service/<name>/` happy-path-scripts. Every Go test
next to the harness (`tests/e2e/<example-slug>_test.go`) paired with the catalog
fixtures+expectations `tests/e2e/<example-slug>/`. Service-fixture is not duplicated
in `tests/e2e/` - Keeper reads it from `examples/service/<name>/` via git-loader
([ADR-039](../adr/0039-e2e-testing.md) §5).

Current coverage:

| Test | Service | Script | Covers |
|---|---|---|---|
| `smoke_nginx_test.go` | `smoke-nginx` | `create` | pilot; nginx install + service start. |
| `hello_world_test.go` | `hello-world` | `create` | `input.greeting` (required) + mutation `state.greeting_file`. |
| `noop_test.go` | `noop` | `create` | no-op `core.exec.run`, state is not mutated. |
| `coven_probe_test.go` | `coven-probe` | `create` | init marker, double mutation state (`marker_file`, `last_target`). |
| `long_runner_test.go` | `long-runner` | `create` | init marker, double mutation state (`last_run`, `runner_marker`). |

"Advanced" scenarios (`mark_a/mark_ab/mark_where` coven targeting,
`stagger/serial_waves` long-runner, redis compositions with Vault + destiny-deps,
sentinel multi-host) require multi-host fixture / poll mid-run / real
Vault substrates are a separate slice, not included in the current L3a extension.

## See also

- [ADR-039](../adr/0039-e2e-testing.md) is the solution.
- [README of test levels](README.md) - index L0/L1/L2/L3a/L3b/L3c.
- [`tests/e2e/README.md`](../../tests/e2e/README.md) — L3a quickstart.
- [`tests/e2e-live/README.md`](../../tests/e2e-live/README.md) — L3b quickstart.
- [`tests/e2e-k8s/README.md`](../../tests/e2e-k8s/README.md) — L3c quickstart.
