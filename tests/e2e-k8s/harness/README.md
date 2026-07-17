# tests/e2e-k8s/harness

L3c kind-cluster harness. The architectural contract is symmetric to L3a (`tests/e2e/harness/`)
and L3b (`tests/e2e-live/harness/`): a high-level `Stack` spins up an isolated
stand for a single test, registers teardown via `t.Cleanup`.

## Files

| File | Purpose |
|---|---|
| `cluster.go` | `Cluster` — a wrapper around `sigs.k8s.io/kind/pkg/cluster.Provider`. Create/Delete of a kind cluster, per-test name + temp kubeconfig. |
| `kubectl.go` | `Cluster.KubectlApply` / `Cluster.LoadDockerImage` — subprocess wrappers over the `kubectl` and `kind` CLI. |
| `helm.go` | `Cluster.helmRepoEnsure` + `helmInstall` — bitnami repo runtime + `helm install --wait`. |
| `vault.go` | `seedVaultSecrets` (PKI mount + soul-seed role + JWT signing key + PG DSN) + `IssueKeeperServerCert` for the in-cluster DNS name. |
| `portforward.go` | `Cluster.PortForward(target, remotePort)` — a `kubectl port-forward` subprocess, parsed local port. |
| `keeperyml.go` | `renderKeeperYAML(inputs)` — a static keeper.yml template for the ConfigMap (in-cluster service DNS + TLS paths). |
| `stack.go` | `Stack` — the high-level harness; `NewStack` → `DeployInfra` → `DeployKeeper` (L3c-2 single replica). L3c-3 will extend to multi-replica + `DeploySoul`. |

## Pre-flight

`NewCluster` skips the test (`t.Skip`) if `docker` or the `kind` CLI aren't in PATH.
`docker` is needed by kind internally for the node containers; the `kind` CLI is needed alongside
the Go API for `kind load docker-image` — kind's Go API for load-image
is experimental, the reliable path is the CLI.

## Isolation between tests

- Cluster name: `soul-stack-e2e-<sanitized-test-name>-<unix-nanos>` (PM decision).
  Per-test, not shared — parallel CI runs don't conflict.
- Kubeconfig: `t.TempDir()/kubeconfig-<name>` — automatically removed by the
  test harness.
- `t.Cleanup` (LIFO) guarantees cluster deletion even on `t.Fatal` mid-test.

## Why duplicate the L3a/L3b harness

Architect verdict `a241beb181086d7a7`: `tests/e2e/harness/` and
`tests/e2e-live/harness/` are unreachable from `tests/e2e-k8s/` under Go's
internal-package rules (a different module root). L3a/L3b/L3c are independent
test frequencies (PR / nightly / weekly) with different contracts; shared
code will be extracted into `tests/e2e-shared/` as a separate slice once
all three levels stabilize.

## See also

- [`tests/e2e-k8s/README.md`](../README.md) — quickstart + L3c slice map.
- [docs/testing/e2e.md](../../../docs/testing/e2e.md) — the L3a canonical
  fixtures/expectations format (L3c will read the L3b format with extensions for k8s
  resources, starting with L3c-3).
