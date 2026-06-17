# Примеры Soul Stack

Конкретные иллюстрации того, как выглядят артефакты Soul Stack в реальности. Эти примеры — **не работающий код**, а образцы структуры файлов и YAML-форматов под текущую архитектуру (см. [docs/architecture.md](../docs/architecture.md)).

Цель — чтобы при чтении архитектуры был под рукой «вот как это выглядит на практике».

## Содержимое

| Папка | Что |
|---|---|
| [`destiny/destiny-redis/`](destiny/destiny-redis/) | Атомарный destiny-кирпичик «как поставить и настроить Redis на хосте». Отдельный git-репо в реальной жизни. Включает `tasks/main.yml` (top-level список задач без обёртки) и [`tests/install-and-ping/case.yml`](destiny/destiny-redis/tests/install-and-ping/case.yml) — иллюстрация формата molecule-style тестов destiny. Полный разбор формата — в [docs/destiny/](../docs/destiny/README.md). |
| [`service/service-redis-cluster/`](service/service-redis-cluster/) | Полный пример service-репо: `service.yml`, иерархический `essence/`, набор сценариев, миграции, тесты. |
| [`keeper/keeper.yml`](keeper/keeper.yml) | Конфиг центрального инстанса `keeper` (HA-кластерный, поверх Postgres+Redis+Vault). |
| [`soul/soul.yml`](soul/soul.yml) | Конфиг агента `soul` на управляемом хосте: fallback-list endpoints, retry, failback. |
| [`incarnation/`](incarnation/) | Примеры API-вызовов оператора: создание incarnation, запуск сценария, upgrade. |
| [`module/soul-mod-redis-failover/`](module/soul-mod-redis-failover/) | Скелет custom-модуля для Destiny: манифест и интерфейс (без полной реализации). |

## Поведение примеров

- **YAML без секретов.** Все пароли/токены — через `vault:secret/...` ссылки.
- **Имена хостов** — `*.example`, чтобы не путаться с реальными.
- **Версии** — git-tag-и в `ref:` фиктивные, иллюстративные (см. [ADR-007](../docs/adr/0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) — поля `version:` в манифестах нет).
- Если что-то в архитектуре поменяется — примеры обновляются вместе с ней.
