# official.postgres-user

> ⚠ **`official.*` is the OLD address form (NIM-765) — and it is what ships today.** The
> origin-grouping level of a plugin address is removed: a plugin step is
> `<plugin-name>.<object>.<action>` ([address rule](../../../naming-rules.md#the-discipline-binding-the-three-levels)),
> and `official` named this plugin's origin, not what it manages. **No follow-up ticket** — the
> artifact lives in the companion repo `soul-stack-plugins`, which this repository cannot edit.

Idempotent management of PostgreSQL ROLE (CREATE / ALTER / DROP).

Full documentation of the module (params, state table, output circuit, test coverage, assembly) - [`soul-mod-official-postgres-user/README.md`](https://github.com/souls-guild/soul-stack-plugins/blob/main/soul-mod-official-postgres-user/README.md) in the companion repo.

## Briefly

- **States:** `present` (CREATE/ALTER to delta/no-op), `absent` (DROP/no-op).
- **Probe:** `pg_roles` view (attributes `rolsuper`/`rolcreatedb`/`rolcreaterole`/`rolvaliduntil`).
- **Backend:** `pgx/v5` (parity with keeper side).
- **DSN/password:** secret-input with `pattern: "^vault:.*"` - the operator must use vault-ref, keeper-side vault-resolve resolve BEFORE Apply.

## See also

- [Official plugins directory](../README.md).
- [ADR-016 amendment 2026-05-27 SDK-2](../../../adr/0016-parity-license.md).
