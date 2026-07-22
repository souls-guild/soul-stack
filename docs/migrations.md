# State_schema migration DSL

Normative specification of the `migrations/<NNN>_to_<MMM>.yml` format in the service repository. The source of truth for the solution is [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl). This document contains a grammar, allowed CEL functions in a migration context, test convention, examples.

## Purpose

State_schema migration converts `incarnation.state` (jsonb in Postgres) from version N to N+1 when upgrading the service (`keeper.incarnation.upgrade name=X to_version=v...`). Migration is a **pure function `state_v<N> → state_v<M>`** without host side-effects, performed on the keeper side in a single PG transaction (see [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) sections (c) atomicity and (e) security).

## File layout

`<service-repo>/migrations/<NNN>_to_<MMM>.yml` - one file = one migration step. The `001 → 002 → 003 → ...` chain is run sequentially by the keeper during upgrade.

```
redis/
├── service.yml                            # state_schema_version: 2
├── migrations/
│   ├── 001_to_002.yml                     # format described below
│   └── 001_to_002/                        # tests this migration
│       └── tests/
│           ├── users-list-to-map.yml
│           ├── single-user.yml
│           ├── empty-users.yml
│           └── preserves-unrelated-fields.yml
└── ...
```

File name: `NNN_to_MMM.yml` - three digits with leading zeros, delimiter `_to_`.

## File structure

```yaml
from_version: 1
to_version: 2

description: >
  Transition from redis_users[] array to map redis_users{name: {acl, state}}
  to support per-user ACL and enabled/disabled flag.

# List of operations. Apply in order. Each operation sees state,
# mutated by previous operations of the same migration.
transform:
  # Atomic operations:
  - rename: { from: state.redis_users, to: state.redis_users_legacy_v1 }

  # CEL expressions in set values:
  - set:
      path: state.maxmemory_bytes
      value: "${ int(state.maxmemory_mb) * 1048576 }"

  - delete: { path: state.maxmemory_mb }

  # Iterate through a collection using a structured foreach.
  - foreach: "${ state.redis_users_legacy_v1 }"
    as: user_name
    do:
      - set:
          path: "state.redis_users.${ user_name }"
          value:
            acl: "off ~* &* +@all"
            state: "off"

  - delete: { path: state.redis_users_legacy_v1 }
```

## Operations `transform:`

| Operation | Options | Semantics |
|---|---|---|
| **`rename`** | `from: <path>`, `to: <path>` | Move the value from `from` to `to`. If `to` already exists, an error occurs (explicit `delete` before rename). |
| **`set`** | `path: <path>`, `value: <yaml>` or `<CEL expression>` | Write `value` to `path`. If the key exists, it is overwritten. `value` can be a YAML literal (map/list/scalar) or a CEL expression via `${ … }` or a nested structure with built-in `${ … }` interpolations. |
| **`delete`** | `path: <path>` | Remove value by `path`. If it does not exist, no-op (not an error). |
| **`move`** | `from: <path>`, `to: <path>` | Alias for `rename` (historical; same semantics). |
| **`foreach`** | `in: <CEL expression>` (or short form `foreach: <CEL expression>`), `as: <var-name>`, `do: [<operation>, ...]` | Structural loop: iteration through the list/map values, at each step `<var-name>` is bound to the current element. `do:` - nested transform list. Inside `do:`, `<var-name>` and the entire current `state.*` are available. |

**The operations list is now closed** (`rename`/`set`/`delete`/`move`/`foreach`). Conditional `if:` key - on post-MVP (see [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl), option (c) target).

## Addressing - `path:`

Dot notation from the root of the state object: `state.foo`, `state.bar.baz`, `state.users.${ name }.acl`.

- The `state.` prefix is required (explicit scope).
- Path segments are letters/numbers/`_`/`-` or `${ <CEL> }`-interpolation.
- Access to an array element by index: `state.hosts.0.ip` (in MVP it is not used in the examples - it will be added if necessary).

## CEL in the migration context

Any value in `set.value`, `foreach.in`, `path:` supports CEL expressions through the `${ … }` marker ([ADR-010](adr/0010-templating.md)).

### Available variables

| Name | Type | Semantics |
|---|---|---|
| `state` | object | Current state (mutated during operations). The root value is `incarnation.state`. |

Inside `foreach.do[*]` additionally:

| Name | Type | Semantics |
|---|---|---|
| `<as-name>` | dyn | The current element of the iteration (value if `in` is map; list element if `in` is list). |

### Available CEL functions

Standard CEL functions (`int`, `string`, `bool`, `size`, `has`, comprehensions `map`/`filter`/`all`/`exists`/`exists_one`) + operators (`+`/`-`/`*`/`/`/`==`/`!=`/`<`/`>`/`<=`/`>=`/`&&`/`||`/`!`/`in`/`?:`).

migration-CEL - sandbox with minimal surface area: stdlib only (only the variable `state` is declared). `glob()`/`merge()`/`default()` and any pure extensions of regular CEL are **not** registered here (the extension requires a separate ADR). `keys()`/`values()` is **not in this list** - they are not in migration-CEL, `${ keys(...) }` crashes during compilation (`undeclared reference`).

To iterate through the map, the native macro `.map()` **above the map itself** is used: it bypasses the **keys** (iteration element = key), the value is obtained by the index `m[k]`. This is how `map → array` is collapsed without `keys()` - see migration [`examples/service/redis/migrations/005_to_006.yml`](../examples/service/redis/migrations/005_to_006.yml) (`state.redis_users.map(n, {'name': n, 'perms': state.redis_users[n].perms, ...})`).

### Not allowed in migration-CEL

| Name | Why |
|---|---|
| `vault(...)` | Migration should not involve secrets. |
| `now()` | For test reproducibility. |
| `register.*` | There is no host context (migration - keeper-side). |
| `soulprint.*` | Likewise. |
| `essence.*` | Migration should be a pure function of the old state, not dependent on the current essence. |
| `input.*` | Migration does not accept operator-parameters (only `state`). |
| Any user-defined CEL functions | Sandbox by design. |

## Reverse / downgrade

In MVP - **forward only**. Incident recovery is via `state_history` snapshot (see [`docs/architecture.md → state_history`](architecture.md#state_history--state-change-log)).

Optional `down:` block in the migration file can be added post-MVP without breaking change. The current grammar does not support this block.

## Atomicity

The migration chain `<from_version>` → `<to_version>` keeper executes in **one PG transaction**:

1. `BEGIN`.
2. `SELECT state, state_schema_version FROM incarnation WHERE name = ? FOR UPDATE`.
3. Apply migrations sequentially in memory (Go): `state_v1 → state_v2 → state_v3 → ...`.
4. At each step `INSERT INTO state_history (state_before, state_after, scenario, changed_by_aid, ...)` with `scenario: "migration"`.
5. `UPDATE incarnation SET state = ?, state_schema_version = ?, service_version = ?`.
6. `COMMIT`.

If any step fails - `ROLLBACK`, the incarnation is marked `status: migration_failed` ([architecture.md → §"Versioning and migration state_schema"](architecture.md#versioning-and-state_schema-migrations)).

## Testing

Migration tests live in `migrations/<NNN_to_MMM>/tests/<case>.yml`. Format:

```yaml
name: redis-users-array-to-map
description: >
  Base case: an array of names goes into a map with a per-user ACL.

state_before:
  redis_users: ["app", "monitor"]
  maxmemory_mb: 512

state_after:
  redis_users:
    app:     { acl: "off ~* &* +@all", state: "off" }
    monitor: { acl: "off ~* &* +@all", state: "off" }
  maxmemory_bytes: 536870912
```

Test:
1. Loads `state_before` as `state`.
2. Applies migration operations.
3. Checks the resulting `state` against `state_after` (deep-equal).

Triggered via `soul-trial <service-repo>/migrations/<NNN_to_MMM>/` ([ADR-023](adr/0023-trial-test-runner.md): "executes → soul-trial", as opposed to the purely static `soul-lint`). The runner mechanics are a separate task after the spec.

## Related Documents

- [ADR-019 in `docs/architecture.md`](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) - committing the solution.
- [ADR-009 in `docs/architecture.md`](adr/0009-scenario-dsl.md) - old mention of flat DSL (now replaced by ADR-019).
- [ADR-010 in `docs/architecture.md`](adr/0010-templating.md) - CEL as a single expression engine.
- [`docs/architecture.md` → §"Versioning and migrations state_schema"](architecture.md#versioning-and-state_schema-migrations) - high-level description (`state_schema_version`, upgrade mechanism, atomicity).
- [`docs/architecture.md` → §"`state_history`"](architecture.md#state_history--state-change-log) - a log through which recovery in the event of an incident is available.
- [`docs/templating.md`](templating.md) — CEL general spec.
- [`examples/service/redis/migrations/`](../examples/service/redis/migrations/) - example (migration of `001_to_002`: `redis_users` from the list of names to map `name → {perms, state}` via `foreach`).
