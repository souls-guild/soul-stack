# tests/e2e-k8s — L3c kind-cluster E2E

L3c E2E поверх реального Kubernetes-кластера (kind + bitnami Helm). В отличие
от L3a (`tests/e2e/`, in-process Keeper + soul-stub) и L3b (`tests/e2e-live/`,
real `soul`-binary в privileged docker-контейнере), L3c:

- поднимает реальный K8s-кластер локально через [kind](https://kind.sigs.k8s.io/);
- деплоит инфру (PostgreSQL / Redis / Vault) через bitnami Helm-чарты;
- деплоит Keeper и Soul как раздельные K8s-ресурсы (Deployment / StatefulSet);
- покрывает HA-сценарии, недоступные в L3a/L3b (Watchman / Toll / leader-failover).

Frequency: **weekly / pre-release** (медленный — 5-15 мин на тест из-за
kind-spin-up + helm-install + image-load).

## Запуск

```sh
# Полный прогон L3c (требует docker + kind CLI в PATH)
make e2e-k8s

# Один тест напрямую
cd tests/e2e-k8s && go test -tags=e2e_k8s -run TestL3cKindUp -timeout=30m -p 1 ./...
```

Pre-requisites:

- `docker` (kind использует docker как node-runtime).
- `kind` CLI ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)).
- `kubectl` (port-forward + apply через subprocess; L3c-2+).
- `helm` (L3c-2+).
- Образы `keeper:e2e-k8s` и `soul:e2e-k8s` (собираются через
  `make docker-build-keeper` + `make docker-build-soul`;
  Makefile-таргет `e2e-k8s` уже зависит от обоих).

Без `docker`/`kind`/`kubectl`/`helm` тесты скипаются (`t.Skip`); сборка и
`go vet -tags=e2e_k8s ./...` проходят всегда.

## Раскладка

```
tests/e2e-k8s/
├── README.md                       # этот файл
├── go.mod / go.sum                 # отдельный go-модуль (deps не утекают)
├── harness/
│   ├── cluster.go                  # kind spin-up/teardown через sigs.k8s.io/kind/pkg/cluster
│   ├── kubectl.go                  # subprocess kubectl apply / kind load docker-image
│   ├── helm.go                     # subprocess helm repo add + helm install (L3c-2)
│   ├── vault.go                    # PKI mount + JWT signing-key + PG DSN seed + leaf-cert issue (L3c-2)
│   ├── portforward.go              # `kubectl port-forward svc/<x> :<port>` host-side туннели (L3c-2)
│   ├── keeperyml.go                # рендер keeper.yml для ConfigMap (L3c-2)
│   ├── stack.go                    # высокоуровневый Stack: NewStack/DeployInfra/DeployKeeper
│   ├── jwt.go                      # L3c-5: BootstrapArchon через exec `keeper init`
│   ├── multisoul.go                # L3c-5: DeployMultiSoul (N независимых Pod-ов)
│   ├── scenario.go                 # L3c-5: CreateIncarnation / RunScenario / PostScenarioRaw / WaitApplySuccess
│   ├── toll.go                     # L3c-5: IsTollDegraded / WaitForTollDegraded (Redis EXISTS)
│   └── README.md
├── manifests/
│   ├── keeper/deployment.yaml      # raw YAML: Deployment + Service (L3c-2)
│   └── soul/                       # raw YAML: StatefulSet + headless Service + systemd-unit (L3c-3)
│       ├── statefulset.yaml
│       └── soul.service            # systemd-unit для soul-агента (baked в soul:e2e-k8s)
├── helm-values/                    # bitnami values для PG/Redis/Vault (L3c-2)
│   ├── postgres.yaml
│   ├── redis.yaml
│   └── vault.yaml
├── dockerfiles/
│   ├── keeper.Dockerfile           # distroless-runtime поверх make build-linux артефакта (L3c-2)
│   └── soul.Dockerfile             # privileged systemd-PID-1 Debian-12 (parity с L3b, L3c-3)
├── smoke_kind_up_test.go              # L3c-1 pilot smoke
├── keeper_ping_test.go                # L3c-2: TestL3cKeeperPing_Single (/readyz=200)
├── multi_keeper_bootstrap_test.go     # L3c-3: TestL3cMultiKeeper_BootstrapAndConnect
├── multi_keeper_failover_test.go      # L3c-4: TestL3cMultiKeeper_KillLeader
├── toll_degraded_test.go              # L3c-5 part A: TestL3cToll_DegradedMode
└── redis_cluster_resharding_test.go   # L3c-5 part B: t.Skip каркас (нужен git-server-pod в kind)
```

Дополнительно в L3c-4 в `harness/`:

- `leader.go` — `GetReaperLeaderKID` / `GetSoulLeaseHolder` / `FindKeeperPodByKID`
  / `WaitForDeploymentReady` / `DeleteKeeperPod` (введён для kill-pod-сценария).
- `keeperyml.go::reaperBlock` — условный `reaper:` блок с lock_ttl=15s под
  `Config.ReaperEnabled`.
- `manifests/keeper/deployment.yaml` — init-container `kid-render`, который
  подставляет `$POD_NAME` в `__KID__` шаблона. Без этого 3 pod имели бы
  одинаковый KID и failover detection через `GET reaper:leader` не различал
  бы лидеров.

## Slices

L3c реализуется итеративно. Карта slice-ов (architect-вердикт `a241beb181086d7a7`):

| Slice | Содержание | Статус |
|---|---|---|
| **L3c-1** | Раскладка + Dockerfile-placeholder + `make e2e-k8s` + `harness.NewCluster` (kind spin-up через Go-API) + `TestL3cKindUp_Empty` pilot. | **done** |
| **L3c-2** | `Stack.DeployInfra` (bitnami Helm PG/Redis/Vault) + `Stack.DeployKeeper` (single keeper-pod, distroless, `--initialize` mode, raw YAML Deployment+Service+ConfigMap+Secret) + Vault PKI/JWT/DSN seed через port-forward + `TestL3cKeeperPing_Single` (port-forward keeper:8080 → /readyz=200). | **done** |
| **L3c-3** | `Stack.DeployKeeper` replicas: 3 (HA) + `Stack.DeploySoul` (StatefulSet privileged systemd-PID-1, parity с L3b) + реальный CSR Bootstrap-flow (IssueBootstrapToken через port-forward к PG → `soul init` + `systemctl start soul.service` через client-go remotecommand-exec) + `TestL3cMultiKeeper_BootstrapAndConnect`. | **done** |
| **L3c-4** | HA-failover-сценарий: `TestL3cMultiKeeper_KillLeader` (per-pod KID через init-container + `reaper.enabled` с коротким lock_ttl + `kubectl delete pod --grace-period=0` лидера → дождаться нового лидера в `GET reaper:leader` → Deployment self-heal → Soul реконнект). | **done** |
| **L3c-5** | Toll degraded-mode (`TestL3cToll_DegradedMode`: 5 souls → kill 3 → cluster:degraded → POST scenario=503 + Retry-After + audit `cluster.degraded_set`) + Redis-cluster resharding (`TestL3cRedisCluster_Resharding`: каркас+t.Skip до in-cluster git-server-pod infra — нужен propose-and-wait для service-loader-а в kind). | **done** (part A) / **deferred** (part B) |

## PM-decisions L3c

- **Docker base** для всех будущих L3c images: `gcr.io/distroless/static`
  (для Keeper) — минимальный attack-surface, без shell/dependencies.
- **Soul-pod**: privileged systemd-PID-1 (parity с L3b
  `tests/e2e-live/dockerfiles/debian-12.Dockerfile`); реализуется в L3c-3.
- **kind-cluster name**: per-test
  (`soul-stack-e2e-<sanitized-test-name>-<unix-nanos>`).
- **bitnami repo**: `helm repo add` runtime + retry (vendoring чартов в репо
  отложено).

## Изоляция и cleanup

- Каждый тест поднимает **свой** kind-cluster с unique name → параллельные
  прогоны в CI не конфликтуют.
- Teardown через `t.Cleanup` (LIFO): сначала helm-uninstall (L3c-2+),
  потом `provider.Delete` kind-кластера.
- `-p 1` в Makefile-таргете — serial-прогон тестов (RAM-heavy: kind-cluster
  + bitnami Helm одновременно убьют ноутбук). По умолчанию `go test` ставит
  `-p=GOMAXPROCS`, что для L3c слишком агрессивно.

## Почему harness-код дублируется с L3a/L3b

Architect-вердикт `a241beb181086d7a7`: `tests/e2e/harness/` и
`tests/e2e-live/harness/` под Go-internal-rules недоступны из
`tests/e2e-k8s/` (другой module-root). Дублирование приемлемо, потому что
L3a/L3b/L3c — **независимые test-frequencies** (PR / nightly / weekly) с
разными контрактами (in-process keeper / privileged container / k8s pod).
После стабилизации всех трёх — экстракция общего ядра в `tests/e2e-shared/`
отдельным slice-ом.

## См. также

- [ADR-039](../../docs/adr/0039-e2e-testing.md#adr-039-e2e-тестирование--три-уровня-без-новой-сущности-словаря) — E2E три уровня.
- Architect-вердикт `a241beb181086d7a7` — 5-slice карта L3c.
- [docs/testing/README.md](../../docs/testing/README.md) — индекс L0/L1/L2/L3a/L3b/L3c.
- [tests/e2e/README.md](../e2e/README.md) — L3a quickstart.
- [tests/e2e-live/README.md](../e2e-live/README.md) — L3b quickstart.
