# official.postgres-user

Idempotent-управление PostgreSQL ROLE (CREATE / ALTER / DROP).

Полная документация модуля (params, state-таблица, output-схема, тестовое покрытие, сборка) — [`soul-mod-official-postgres-user/README.md`](https://github.com/co-cy/soul-stack-plugins/blob/main/soul-mod-official-postgres-user/README.md) в companion-репо.

## Кратко

- **States:** `present` (CREATE/ALTER к дельте/no-op), `absent` (DROP/no-op).
- **Probe:** `pg_roles` view (атрибуты `rolsuper`/`rolcreatedb`/`rolcreaterole`/`rolvaliduntil`).
- **Backend:** `pgx/v5` (parity с keeper-стороной).
- **DSN/password:** secret-input с `pattern: "^vault:.*"` — оператор обязан использовать vault-ref, keeper-side vault-resolve резолвит ДО Apply.

## См. также

- [Каталог official-плагинов](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack).
