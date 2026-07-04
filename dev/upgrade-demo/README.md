# upgrade-demo — живой стенд фичи NIM-34 (upgrade v2)

Демо-сервис + скрипт для **ручного теста апгрейда инкарнаций** ([ADR-0068](../../docs/adr/0068-service-upgrade-v2.md)):
`GET /v1/incarnations/{name}/upgrade-paths` (+`?to=`) и `POST .../upgrade`. Показывает
все ветви резолва — cheap / found / legacy / state-миграции — и живой legacy-upgrade
до `drift`.

Зачем отдельная фикстура: ни один `examples/service/*` не несёт каталога
`upgrade/<slug>/` и не имеет нескольких тегов-версий, поэтому фичу на них не показать.
Фикстура живёт в `dev/` (не в `examples/`), чтобы не влиять на trial/soul-lint примеров
и `make check`.

## Что демонстрирует

Сервис `upgrade-demo` с тремя тегами (собираются скриптом в git-репо
`/tmp/keeper-dev/repos/upgrade-demo` из снапшотов `tree/`):

| Тег | schema | `upgrade/` | Роль в демо |
|---|---|---|---|
| `v1.0.0` | 1 | нет | стартовый пин bare-инкарнации |
| `v2.0.0` | 2 | `upgrade/to_v2/` (`from: ["v1.0.0"]`) | цель режима **found** |
| `v2.0.1` | 2 | нет | цель режима **legacy** (та же миграция `001_to_002`) |

Инкарнация создаётся **bare** (у сервиса нет create-сценария → без хостов, сразу
`ready` на пине `v1.0.0`, `state_schema_version=1`).

## Покрытые кейсы (доказываются прогоном)

- **cheap** — `GET .../upgrade-paths` без `?to=`: список тегов реестра
  (`v1.0.0`/`v2.0.0`/`v2.0.1`/`main`) + `is_current=true` у `v1.0.0`.
- **found** — `?to=v2.0.0`: `direction=forward`, `mode=found`, `slug=to_v2`,
  `reachable=true`, `state_migrations=[{from:1,to:2,path:migrations/001_to_002.yml}]`
  (сканер нашёл `upgrade/to_v2/main.yml`, чей `from:` содержит текущий пин).
- **legacy** — `?to=v2.0.1`: `mode=legacy` (нет `upgrade/`-сценария, `slug` опущен),
  `reachable=true`, та же цепочка миграций.
- **живой legacy-upgrade** — `POST .../upgrade {to_version:"v2.0.1"}` → `202 {apply_id}`
  → смена пина `v1.0.0→v2.0.1` + миграция state (schema `1→2`, `state.schema_v2=true`)
  → `status=drift`. Одной PG-транзакцией, без хостов.

## Prerequisites

Живой dev-стенд (keeper на `:8080` + PG/Redis/Vault). Один раз:

```bash
make dev-up          # docker PG(5434)/Redis(6381)/Vault(8200)
make dev-provision   # TLS + Vault KV/PKI (дорого — только если ещё не делали)
VAULT_TOKEN=root make dev-keeper   # keeper на :8080 (СОБРАННЫЙ ИЗ ЭТОГО worktree — в нём фича NIM-34)
```

> **★ keeper должен быть собран из этого worktree** (`feat/nim34-upgrade-v2`): роут
> `upgrade-paths` введён этой фичей, в бинаре из `main`/старой сборки его нет. Скрипт
> детектит это и печатает точную команду пересборки. Если keeper уже на `:8080` —
> скрипт его переиспользует.
>
> **★ VAULT_TOKEN**: dev-скрипты берут `VAULT_TOKEN` из env; если там прод-токен
> (частая грабля), форсируй `VAULT_TOKEN=root`. `run.sh` делает это сам.

## Запуск

```bash
bash dev/upgrade-demo/run.sh
```

Идемпотентен: git-репо/реестр пересоздаются безопасно; keeper переиспользуется, если
уже на `:8080`. Каждый прогон создаёт новую инкарнацию `updemo-<rand>` (команда сноса —
в конце вывода). Скрипт печатает фактические curl-ответы по каждому кейсу и падает с
понятной ошибкой, если стена (нет keeper/фичи/Vault).

## Что НЕ покрыто

- **found-автозапуск** (`POST upgrade` на `v2.0.0`): found-ветвь после миграции
  запускает upgrade-сценарий на хостах (`Runner.Start` → `applying` → `ready`). Это
  требует поднятых souls — уровень e2e-live, вне dev-стенда. Здесь `?to=v2.0.0` лишь
  **показывает**, что переход был бы `found` (сканер + `slug`); сам upgrade гоняется
  только по legacy-пути (`v2.0.1`, без хостов).
- Авто-чейнинг `v1→v3`, glob/semver в `from:`, массовый апгрейд (NIM-35) — non-goals
  MVP ([ADR-0068 §8](../../docs/adr/0068-service-upgrade-v2.md)).

## Структура

```
dev/upgrade-demo/
  run.sh                     # идемпотентный прогон (boot/reuse → сборка → сев → кейсы)
  README.md                  # этот файл
  tree/
    v1.0.0/service.yml + essence/_default.yaml
    v2.0.0/… + migrations/001_to_002.yml + upgrade/to_v2/main.yml   (found)
    v2.0.1/… + migrations/001_to_002.yml                            (legacy)
```
