# Soul Stack Web — внешний компонент

Web-UI для Soul Stack живёт в **отдельном репозитории** [`soul-stack-web`](https://github.com/co-cy/soul-stack-web) (предполагаемое имя; remote ещё не заведён).

Это аналог разделения **SaltStack ↔ salt-manager** / **OpenStack ↔ Horizon** / **Kubernetes ↔ Dashboard**: ядро отдаёт OpenAPI/MCP, UI потребляет контракт.

## Почему отдельный репо
- **Разные релизные циклы.** Ядро Soul Stack (Go) и UI (TS+React) обновляются с разной частотой.
- **Разные зависимости.** UI тянет node_modules (~400+ пакетов), ядро — Go. Не смешиваем.
- **Опциональный компонент.** Operator может работать через CLI (`soulctl`), MCP, OpenAPI без UI.
- **Параллельная команда.** UI-разработка может вестись фронтенд-командой независимо от core.

## Связь с core
- **Контракт:** [`docs/keeper/openapi.yaml`](../keeper/openapi.yaml) — единственный публичный API-контракт.
- **TS-клиент:** UI генерирует типы через `openapi-typescript` (см. `soul-stack-web/scripts/gen-api.sh`).
- **Аутентификация:** JWT ([ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта), [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)) — UI получает JWT через operator-bootstrap или планируемый `/v1/auth/login` endpoint.

## Что делать при разработке UI
Если меняется OpenAPI-контракт — UI-команда:
1. Тянет обновлённый `openapi.yaml` из core-репо (раннее — copy в `vendor/openapi.yaml`; позже — submodule или published artifact).
2. Запускает `npm run gen:api` → обновляет `src/api/types.gen.ts`.
3. Обновляет UI-страницы под новый контракт.

См. [ADR-035](../adr/0035-distribution-split.md#adr-035-distribution-split--core-apicli-vs-web-ui) — формальное архитектурное решение.
