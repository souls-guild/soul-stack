# tests/e2e-k8s/harness

L3c kind-cluster harness. Архитектурный контракт симметричен L3a (`tests/e2e/harness/`)
и L3b (`tests/e2e-live/harness/`): высокоуровневый `Stack` поднимает изолированный
стенд под один тест, регистрирует teardown через `t.Cleanup`.

## Файлы

| Файл | Назначение |
|---|---|
| `cluster.go` | `Cluster` — обёртка `sigs.k8s.io/kind/pkg/cluster.Provider`. Create/Delete kind-кластера, per-test name + temp kubeconfig. |
| `kubectl.go` | `Cluster.KubectlApply` / `Cluster.LoadDockerImage` — subprocess-обёртки над `kubectl` и `kind` CLI. |
| `helm.go` | `Cluster.helmRepoEnsure` + `helmInstall` — bitnami repo runtime + `helm install --wait`. |
| `vault.go` | `seedVaultSecrets` (PKI mount + role soul-seed + JWT signing-key + PG DSN) + `IssueKeeperServerCert` для in-cluster DNS-имени. |
| `portforward.go` | `Cluster.PortForward(target, remotePort)` — `kubectl port-forward` subprocess, parsed-local-port. |
| `keeperyml.go` | `renderKeeperYAML(inputs)` — статический шаблон keeper.yml для ConfigMap (in-cluster service-DNS + TLS-paths). |
| `stack.go` | `Stack` — высокоуровневый harness; `NewStack` → `DeployInfra` → `DeployKeeper` (L3c-2 single replica). L3c-3 расширит на multi-replica + `DeploySoul`. |

## Pre-flight

`NewCluster` скипает тест (`t.Skip`), если в PATH нет `docker` или `kind` CLI.
`docker` нужен kind-у внутри для node-контейнеров; `kind` CLI нужен помимо
Go-API для `kind load docker-image` — Go-API kind-а для load-image
экспериментальный, надёжный путь — CLI.

## Изоляция между тестами

- Имя кластера: `soul-stack-e2e-<sanitized-test-name>-<unix-nanos>` (PM-decision).
  Per-test, не shared — параллельные прогоны в CI не конфликтуют.
- Kubeconfig: `t.TempDir()/kubeconfig-<name>` — автоматически удаляется
  test-harness-ом.
- `t.Cleanup` (LIFO) гарантирует delete-cluster даже при `t.Fatal` в середине
  теста.

## Почему дублирование с L3a/L3b harness

Architect-вердикт `a241beb181086d7a7`: `tests/e2e/harness/` и
`tests/e2e-live/harness/` под Go-internal-rules недоступны из
`tests/e2e-k8s/` (другой module-root). L3a/L3b/L3c — независимые
test-frequencies (PR / nightly / weekly) с разными контрактами; общий
код экстрактится в `tests/e2e-shared/` отдельным slice-ом после
стабилизации всех трёх уровней.

## См. также

- [`tests/e2e-k8s/README.md`](../README.md) — quickstart + L3c slice-карта.
- [docs/testing/e2e.md](../../../docs/testing/e2e.md) — L3a-канон формата
  fixtures/expectations (L3c будет читать формат L3b с расширениями под k8s
  ресурсы, начиная с L3c-3).
