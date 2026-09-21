# manifests/keeper

Raw YAML manifests for Keeper in the L3c kind-cluster.

| File | Contains | Slice |
|---|---|---|
| `deployment.yaml` | `Deployment` (replicas: 3, multi-keeper HA by default) + `Service` (ClusterIP, openapi/mcp/metrics/grpc-bootstrap/grpc-stream). | L3c-2/3 |

## Objects NOT in this file

`ConfigMap keeper-config` and `Secret keeper-tls` are created by the harness (see
`tests/e2e-k8s/harness/stack.go::DeployKeeper`) BEFORE `kubectl apply -f
deployment.yaml`:

- `keeper-config.data["keeper.yml"]` — rendered by
  `tests/e2e-k8s/harness/keeperyml.go::renderKeeperYAML` with in-cluster
  service-DNS addresses (PG/Redis/Vault) and ports.
- `keeper-tls.data` — `keeper.crt`/`keeper.key`/`vault-ca.crt`, issued via
  Vault PKI `pki/issue/soul-seed` (see `harness/vault.go::seedVaultSecrets`).

Without them the pod fails with `CreateContainerConfigError` — visible in `t.Log`
via `dumpKeeperPodDiagnostics` (called on the `waitDeploymentReady` timeout).

## Replicas

The manifest is committed with `replicas: 3` (L3c-3 multi-keeper HA by default).
`DeployKeeper(t, replicas)` patches `spec.replicas` via `kubectl patch
deployment` if a different value is requested (e.g. the legacy
single-keeper `TestL3cKeeperPing_Single` patches it to 1).

## Probes

- `readinessProbe: /readyz` (initialDelay 30s, period 5s, failureThreshold 12 =
  up to 60s of waiting) — `/readyz` pings PG+Vault+Redis. PG migrations are
  applied at startup, so the first successful probe is no earlier than 5-10s.
- `livenessProbe: /healthz` (initialDelay 60s, period 10s) — static 200, only
  catches a full process hang.

## `--initialize` for L3c-2

`Dockerfile::CMD` runs `keeper run --config /etc/keeper/keeper.yml
--initialize` — bootstrap-pending mode (ADR-013(d)): keeper starts with an empty
operators registry, OperatorAPI responds 503 `operator-bootstrap-pending`, but
`/readyz` and `/healthz` respond normally. For the smoke test
`TestL3cKeeperPing_Single` this is enough. L3c-3 will replace this with a full
bootstrap (kubectl job with `keeper init` + retry-Deployment without
`--initialize`).

See [`tests/e2e-k8s/README.md`](../../README.md) -> slice L3c-2.
