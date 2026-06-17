# soul-legion — нагрузочный генератор Soul Stack

Test-only инструмент (НЕ поставочный бинарь — [ADR-004](../../docs/adr/0004-binaries.md)
фиксирует только `keeper`/`soul`/`soul-lint`). Поднимает N одновременных
fake-Soul-стримов (gRPC bidi поверх mTLS `EventStream`) к живому Keeper-у и мерит
нагрузку **на Keeper**, а не реализм apply на хосте.

Нормативный план, методика и измеренные числа — [docs/testing/load-testing.md](../../docs/testing/load-testing.md).
Этот README — только про запуск.

## Запуск: `make stress`

Из корня репо, на **поднятом dev-стенде**:

```sh
make stress                          # ось A: 1000 коннектов, cleanup
make stress COUNT=500 API=1 VOYAGE=1 # + ось B (API) + ось C (один Voyage)
make stress COUNT=2000 RAMP=500 DURATION=60s
```

Таргет сам собирает бинарь (`tests/load/bin/soul-legion`), при `API=1`/`VOYAGE=1`
минтит admin-JWT тем же механизмом, что `make dev-jwt` (`dev/mint-jwt.sh`, ключ из
Vault — не дублируется), прогоняет soul-legion и чистит легион из реестра
(`--cleanup`, стенд остаётся чистым).

`load-test` — алиас `stress`.

### Предусловие

Нужен поднятый dev-стенд: Keeper (event-stream `:9443`, metrics `:9090`, openapi
`:8080`) + dev-PKI (`/tmp/keeper-dev/tls/vault-ca.crt`) + PG/Redis/Vault из
docker-compose. Таргет проверяет `/healthz` до сборки; при недоступности
подсказывает `make dev-stand` (полный подъём) или `make dev-keeper` (только keeper).

### Что меряет

- **Ось A (всегда):** масса EventStream-стримов — achieved-N, connect-латентность
  p50/p99/max, удержание стримов, дрейн (утечка стримов/горутин), RSS/горутины/FD
  Keeper-а с экстраполяцией по [scaling.md](../../docs/operations/scaling.md).
- **Ось B (`API=1`):** concurrent-гон `GET /v1/souls` + `POST /v1/voyages/preview`
  поверх легиона — RPS, латентность, ошибки.
- **Ось C (`VOYAGE=1`):** один command-Voyage по `coven` легиона — create- и
  end-to-end-латентность, исход, audit-INSERT-rate.

Часть величин метрик не имеет (Redis lease, PG claim/audit-INSERT, Conclave
live-count) — soul-legion печатает готовые CLI-команды для ручного замера снаружи
(см. [load-testing.md §4.2](../../docs/testing/load-testing.md#42-наблюдательные-пробелы--метрик-нет-мерить-снаружи-на-1-й-фазе)).

## ENV-переменные `make stress`

| Переменная | Default | Назначение |
|---|---|---|
| `COUNT` | `1000` | число fake-Soul-стримов (ось A) |
| `RAMP` | `250` | стримов за ступень ramp-а (0 → все сразу) |
| `RAMP_INTERVAL` | `300ms` | пауза между ступенями |
| `DURATION` | `30s` | сколько держать стримы после полного ramp-а (ось A без B/C) |
| `COVEN` | `legion` | coven-метка легиона (таргет осей B/C) |
| `API` | `0` | `1` → включить ось B (API-нагрузка) |
| `VOYAGE` | `0` | `1` → включить ось C (один Voyage) |
| `API_DURATION` | `15s` | длительность API-гона (ось B) |
| `KEEPER_ENDPOINT` | `127.0.0.1:9443` | Keeper event_stream (mTLS) |
| `OPENAPI` | `http://127.0.0.1:8080` | OpenAPI-listener (оси B/C) |
| `METRICS` | `http://127.0.0.1:9090` | Keeper `/metrics` (пусто → не скрейпить) |
| `PG` | `postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable` | PG DSN (setup/cleanup) |
| `VAULT` | `http://127.0.0.1:8200` | Vault (dev-PKI для batch-issue leaf-сертов) |
| `STRESS_CA` | `/tmp/keeper-dev/tls/vault-ca.crt` | root CA Keeper-server-cert-а (PEM) |

Прочие флаги soul-legion (sid-prefix, open-concurrency, issue-concurrency,
voyage-module и т.д.) имеют вменяемые дефолты в коде — для прямого вызова см.
`go run ./cmd/soul-legion --help` в `tests/load/`.
