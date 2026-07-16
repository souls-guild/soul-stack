# official.postgres-user

Idempotent management of PostgreSQL ROLE (CREATE / ALTER / DROP).

Full documentation of the module (params, state table, output circuit, test coverage, assembly) - [`soul-mod-official-postgres-user/README.md`](https://github.com/co-cy/soul-stack-plugins/blob/main/soul-mod-official-postgres-user/README.md) in the companion repo.

## Briefly

- **States:** `present` (CREATE/ALTER to delta/no-op), `absent` (DROP/no-op).
- **Probe:** `pg_roles` view (attributes `rolsuper`/`rolcreatedb`/`rolcreaterole`/`rolvaliduntil`).
- **Backend:** `pgx/v5` (parity with keeper side).
- **DSN/password:** secret-input with `pattern: "^vault:.*"` - the operator must use vault-ref, keeper-side vault-resolve resolve BEFORE Apply.

## See also

- [Official plugins directory](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md).
