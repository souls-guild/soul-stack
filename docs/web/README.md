# Soul Stack Web — операторский UI

Source-of-truth UI живёт в **отдельном companion-репозитории** [`soul-stack-web`](https://github.com/souls-guild/soul-stack-web) (TS + React): разработка, релизный цикл и node-зависимости — там.

**С [ADR-055](../adr/0055-embed-ui-bundle.md#adr-055-embed-ui-bundle--опциональный-single-binary-keeper-с-ui-на-ui) собранный UI по умолчанию ВСТРОЕН в `keeper`-бинарь** (`go:embed`) и отдаётся им на маршруте `/ui` — отдельного процесса, порта и деплоя не требуется. Это **не** разворот разделения дистрибуции: companion-репо остаётся каноном кода UI, в core попадает лишь завендоренный снапшот собранной статики (`dist/`). Тоггл — [`web_ui_enabled`](../keeper/config.md#web_ui_enabled-top-level) (default-ON, явный `false` — opt-out).

## Где что живёт

| | Где | Что |
|---|---|---|
| **Source UI** | companion `soul-stack-web` | исходники TS+React, npm-зависимости, vite-сборка, релизный цикл фронта |
| **Завендоренный снапшот** | `keeper/internal/webui/assets/` (этот репо) | собранный `dist/` companion-а, вкомпиливается в `keeper` через `go:embed`; отдаётся на `/ui` |
| **API-контракт** | [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) | единственный публичный контракт; UI генерирует TS-типы из него (`openapi-typescript`) |

## Синхронизация снапшота (core ← companion)

Завендоренная статика обновляется из companion-репо двумя Make-таргетами:

| Таргет | Что делает |
|---|---|
| `make sync-webui` | Вендоринг: rsync `--delete` зеркалит собранный `dist/` companion `soul-stack-web` → `keeper/internal/webui/assets/`. Скрипт — `scripts/sync-webui.sh`. |
| `make check-webui` | Drift-guard в `make check`: ошибка, если завендоренная копия разошлась с companion-сборкой («забыли `sync-webui` после изменения UI»). Без доступного companion-репо — skip. |

Типовой цикл при изменении UI: правка в companion → сборка `dist/` там → `make sync-webui` в core → коммит обновлённого снапшота.

## Почему companion остаётся отдельным репо

- **Разные релизные циклы и зависимости.** Ядро (Go) и UI (TS+React, node_modules) обновляются с разной частотой и разным toolchain-ом; смешивать сборки не нужно.
- **Параллельная фронтенд-разработка** независимо от core.
- **UI остаётся опциональным.** Оператор может работать через CLI (`soulctl`), MCP или напрямую OpenAPI; `web_ui_enabled: false` снимает `/ui` без влияния на `/v1/*` и `/docs`.

## Связь с core

- **Контракт:** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) — единственный публичный API-контракт.
- **TS-клиент:** UI генерирует типы через `openapi-typescript` (см. `soul-stack-web/scripts/gen-api.sh`).
- **Аутентификация:** JWT ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта), [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) — оператор вставляет JWT в UI вручную (bootstrap-токен из `keeper init --archon` либо токен из `POST /v1/operators/{aid}/issue-token`); отдельного `/v1/auth/login`-эндпоинта пока нет.

См. [ADR-035](../adr/0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui) (разделение дистрибуции core ↔ UI) и [ADR-055](../adr/0055-embed-ui-bundle.md#adr-055-embed-ui-bundle--опциональный-single-binary-keeper-с-ui-на-ui) (embed на `/ui`, amends ADR-035 п.3).
