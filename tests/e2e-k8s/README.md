# tests/e2e-k8s — L3c kind-cluster E2E

L3c E2E on top of a real Kubernetes cluster (kind + bitnami Helm). Unlike
L3a (`tests/e2e/`, in-process Keeper + soul-stub) and L3b (`tests/e2e-live/`,
a real `soul` binary in a privileged docker container), L3c:

- spins up a real K8s cluster locally via [kind](https://kind.sigs.k8s.io/);
- deploys infra (PostgreSQL / Redis / Vault) via bitnami Helm charts;
- deploys Keeper and Soul as separate K8s resources (Deployment / StatefulSet);
- covers HA scenarios unavailable in L3a/L3b (Watchman / Toll / leader-failover).

Frequency: **weekly / pre-release** (slow — 5-15 min per test due to
kind-spin-up + helm-install + image-load).

## Running

```sh
# Full L3c run (requires docker + kind CLI on PATH)
make e2e-k8s

# A single test directly
cd tests/e2e-k8s && go test -tags=e2e_k8s -run TestL3cKindUp -timeout=30m -p 1 ./...
```

Pre-requisites:

- `docker` (kind uses docker as the node runtime).
- `kind` CLI ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)).
- `kubectl` (port-forward + apply via subprocess; L3c-2+).
- `helm` (L3c-2+).
- Images `keeper:e2e-k8s` and `soul:e2e-k8s` (built via
  `make docker-build-keeper` + `make docker-build-soul`;
  the `e2e-k8s` Makefile target already depends on both).

Without `docker`/`kind`/`kubectl`/`helm` the tests are skipped (`t.Skip`); building and
`go vet -tags=e2e_k8s ./...` always pass.

## Layout

```
tests/e2e-k8s/
├── README.md                       # this file
├── go.mod / go.sum                 # separate go module (deps don't leak)
├── harness/
│   ├── cluster.go                  # kind spin-up/teardown via sigs.k8s.io/kind/pkg/cluster
│   ├── kubectl.go                  # subprocess kubectl apply / kind load docker-image
│   ├── helm.go                     # subprocess helm repo add + helm install (L3c-2)
│   ├── vault.go                    # PKI mount + JWT signing-key + PG DSN seed + leaf-cert issue (L3c-2)
│   ├── portforward.go              # `kubectl port-forward svc/<x> :<port>` host-side tunnels (L3c-2)
│   ├── keeperyml.go                # renders keeper.yml for the ConfigMap (L3c-2)
│   ├── stack.go                    # high-level Stack: NewStack/DeployInfra/DeployKeeper
│   ├── jwt.go                      # L3c-5: BootstrapArchon via exec `keeper init`
│   ├── multisoul.go                # L3c-5: DeployMultiSoul (N independent Pods)
│   ├── scenario.go                 # L3c-5: CreateIncarnation / RunScenario / PostScenarioRaw / WaitApplySuccess
│   ├── toll.go                     # L3c-5: IsTollDegraded / WaitForTollDegraded (Redis EXISTS)
│   └── README.md
├── manifests/
│   ├── keeper/deployment.yaml      # raw YAML: Deployment + Service (L3c-2)
│   └── soul/                       # raw YAML: StatefulSet + headless Service + systemd unit (L3c-3)
│       ├── statefulset.yaml
│       └── soul.service            # systemd unit for the soul agent (baked into soul:e2e-k8s)
├── helm-values/                    # bitnami values for PG/Redis/Vault (L3c-2)
│   ├── postgres.yaml
│   ├── redis.yaml
│   └── vault.yaml
├── dockerfiles/
│   ├── keeper.Dockerfile           # distroless runtime on top of a make build-linux artifact (L3c-2)
│   └── soul.Dockerfile             # privileged systemd-PID-1 Debian-12 (parity with L3b, L3c-3)
├── smoke_kind_up_test.go              # L3c-1 pilot smoke
├── keeper_ping_test.go                # L3c-2: TestL3cKeeperPing_Single (/readyz=200)
├── multi_keeper_bootstrap_test.go     # L3c-3: TestL3cMultiKeeper_BootstrapAndConnect
├── multi_keeper_failover_test.go      # L3c-4: TestL3cMultiKeeper_KillLeader
├── toll_degraded_test.go              # L3c-5 part A: TestL3cToll_DegradedMode
└── redis_cluster_resharding_test.go   # L3c-5 part B: t.Skip scaffold (needs a git-server-pod in kind)
```

Additionally in `harness/` for L3c-4:

- `leader.go` — `GetReaperLeaderKID` / `GetSoulLeaseHolder` / `FindKeeperPodByKID`
  / `WaitForDeploymentReady` / `DeleteKeeperPod` (introduced for the kill-pod scenario).
- `keeperyml.go::reaperBlock` — conditional `reaper:` block with lock_ttl=15s under
  `Config.ReaperEnabled`.
- `manifests/keeper/deployment.yaml` — the `kid-render` init-container, which
  substitutes `$POD_NAME` into the `__KID__` template placeholder. Without this, all 3
  pods would have the same KID and failover detection via `GET reaper:leader` would not
  distinguish between leaders.

## Slices

L3c is implemented iteratively. The slice map (architect verdict `a241beb181086d7a7`):

| Slice | Content | Status |
|---|---|---|
| **L3c-1** | Layout + Dockerfile placeholder + `make e2e-k8s` + `harness.NewCluster` (kind spin-up via the Go API) + `TestL3cKindUp_Empty` pilot. | **done** |
| **L3c-2** | `Stack.DeployInfra` (bitnami Helm PG/Redis/Vault) + `Stack.DeployKeeper` (single keeper pod, distroless, `--initialize` mode, raw YAML Deployment+Service+ConfigMap+Secret) + Vault PKI/JWT/DSN seed via port-forward + `TestL3cKeeperPing_Single` (port-forward keeper:8080 → /readyz=200). | **done** |
| **L3c-3** | `Stack.DeployKeeper` replicas: 3 (HA) + `Stack.DeploySoul` (privileged systemd-PID-1 StatefulSet, parity with L3b) + a real CSR bootstrap flow (IssueBootstrapToken via port-forward to PG → `soul init` + `systemctl start soul.service` via client-go remotecommand-exec) + `TestL3cMultiKeeper_BootstrapAndConnect`. | **done** |
| **L3c-4** | HA-failover scenario: `TestL3cMultiKeeper_KillLeader` (per-pod KID via init-container + `reaper.enabled` with a short lock_ttl + `kubectl delete pod --grace-period=0` on the leader → wait for a new leader in `GET reaper:leader` → Deployment self-heal → Soul reconnect). | **done** |
| **L3c-5** | Toll degraded-mode (`TestL3cToll_DegradedMode`: 5 souls → kill 3 → cluster:degraded → POST scenario=503 + Retry-After + audit `cluster.degraded_set`) + Redis-cluster resharding (`TestL3cRedisCluster_Resharding`: scaffold+t.Skip until in-cluster git-server-pod infra exists — needs propose-and-wait for the service-loader in kind). | **done** (part A) / **deferred** (part B) |

## PM decisions for L3c

- **Docker base** for all future L3c images: `gcr.io/distroless/static`
  (for Keeper) — minimal attack surface, no shell/dependencies.
- **Soul pod**: privileged systemd-PID-1 (parity with L3b
  `tests/e2e-live/dockerfiles/debian-12.Dockerfile`); implemented in L3c-3.
- **kind cluster name**: per-test
  (`soul-stack-e2e-<sanitized-test-name>-<unix-nanos>`).
- **bitnami repo**: `helm repo add` at runtime + retry (vendoring the charts into the
  repo is deferred).

## Isolation and cleanup

- Every test spins up **its own** kind cluster with a unique name → parallel runs in
  CI don't conflict.
- Teardown via `t.Cleanup` (LIFO): first helm-uninstall (L3c-2+),
  then `provider.Delete` of the kind cluster.
- `-p 1` in the Makefile target — serial test runs (RAM-heavy: a kind cluster
  + bitnami Helm running together would kill a laptop). By default `go test` sets
  `-p=GOMAXPROCS`, which is too aggressive for L3c.

## Why harness code is duplicated with L3a/L3b

Architect verdict `a241beb181086d7a7`: `tests/e2e/harness/` and
`tests/e2e-live/harness/` are unreachable from `tests/e2e-k8s/` under Go
internal-package rules (a different module root). Duplication is acceptable because
L3a/L3b/L3c are **independent test frequencies** (PR / nightly / weekly) with
different contracts (in-process keeper / privileged container / k8s pod).
Once all three have stabilized, extracting a shared core into `tests/e2e-shared/` is
a separate slice.

## See also

- [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity) — the three E2E levels.
- Architect verdict `a241beb181086d7a7` — the 5-slice L3c map.
- [docs/testing/README.md](../../docs/testing/README.md) — index of L0/L1/L2/L3a/L3b/L3c.
- [tests/e2e/README.md](../e2e/README.md) — L3a quickstart.
- [tests/e2e-live/README.md](../e2e-live/README.md) — L3b quickstart.
