# manifests/keeper

Raw YAML manifests для Keeper в L3c kind-cluster.

| Файл | Содержит | Slice |
|---|---|---|
| `deployment.yaml` | `Deployment` (replicas: 3, multi-keeper HA по дефолту) + `Service` (ClusterIP, openapi/mcp/metrics/grpc-bootstrap/grpc-stream). | L3c-2/3 |

## Объекты, которые НЕ в файле

`ConfigMap keeper-config` и `Secret keeper-tls` создаются harness-ом (см.
`tests/e2e-k8s/harness/stack.go::DeployKeeper`) ДО `kubectl apply -f
deployment.yaml`:

- `keeper-config.data["keeper.yml"]` — рендеренный
  `tests/e2e-k8s/harness/keeperyml.go::renderKeeperYAML` с in-cluster service-DNS
  адресами (PG/Redis/Vault) и портами.
- `keeper-tls.data` — `keeper.crt`/`keeper.key`/`vault-ca.crt`, выданные через
  Vault PKI `pki/issue/soul-seed` (см. `harness/vault.go::seedVaultSecrets`).

Без них pod падает на `CreateContainerConfigError` — это видно в `t.Log`
по `dumpKeeperPodDiagnostics` (вызывается на timeout-е `waitDeploymentReady`).

## Replicas

Manifest коммитится с `replicas: 3` (L3c-3 multi-keeper HA по дефолту).
`DeployKeeper(t, replicas)` патчит `spec.replicas` через `kubectl patch
deployment`, если запрошено отличающееся значение (например, legacy
single-keeper `TestL3cKeeperPing_Single` патчит в 1).

## Probes

- `readinessProbe: /readyz` (initialDelay 30s, period 5s, failureThreshold 12 =
  до 60s ожидания) — `/readyz` пингует PG+Vault+Redis. Миграции PG применяются
  на старте, поэтому первая успешная проба не раньше 5–10s.
- `livenessProbe: /healthz` (initialDelay 60s, period 10s) — static 200, ловит
  только полный hang процесса.

## `--initialize` для L3c-2

`Dockerfile::CMD` запускает `keeper run --config /etc/keeper/keeper.yml
--initialize` — bootstrap-pending mode (ADR-013(d)): keeper стартует с пустым
operators-registry, OperatorAPI отвечает 503 `operator-bootstrap-pending`, но
`/readyz` и `/healthz` отвечают штатно. Для smoke-теста `TestL3cKeeperPing_Single`
этого достаточно. L3c-3 заменит на полный bootstrap (kubectl-job с `keeper
init` + retry-Deployment без `--initialize`).

См. [`tests/e2e-k8s/README.md`](../../README.md) → slice L3c-2.
