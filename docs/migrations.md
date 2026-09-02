# State_schema migration DSL

Normative specification of the `migrations/<NNN>_<slug>/main.yml` format in the service repository. The source of truth for the solution is [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl). This document contains a grammar, allowed CEL functions in a migration context, test convention, examples.

## Purpose

State_schema migration converts `incarnation.state` (jsonb in Postgres) from version N to N+1 when upgrading the service (`keeper.incarnation.upgrade name=X to_version=v...`). Migration is a **pure function `state_v<N> → state_v<M>`** without host side-effects, performed on the keeper side in a single PG transaction (see [ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) sections (c) atomicity and (e) security).

## File layout

A migration step is a directory `migrations/<NNN>_<slug>/` holding `main.yml` and its `tests/`. The number is the version the step leads to; the "from" is derived — the ladder is forward-only and goes by one. The `1 → 2 → 3 → ...` chain is run sequentially by the keeper during upgrade.

```
<service>/
├── migrations/
│   ├── 002_split_system_and_operator_users/
│   │   ├── main.yml                        # format described below
│   │   └── tests/                          # tests for this step
│   │       ├── splits-system-and-operator-users.yml
│   │       └── idempotent-on-repeat.yml
│   └── schema.lock                         # GENERATED - see "The schema lock"
└── ...
```

Numbering rule: `<NNN>` is three digits with leading zeros and names the version the step **leads to** — `002_…` is the step that takes state from version 1 to version 2. `<slug>` says what the step does and carries no meaning for ordering. A step never states its own `from`: the ladder is forward-only, so the source version is always the number below.

## File structure

```yaml
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

`main.yml` states what the step does and nothing about where it stands. There is no `from_version:` and no `to_version:` — the directory name carries the position, once.

## The schema version

The state-schema version is not stored anywhere — it is the top of the ladder. An empty `migrations/` means version 1.

There is no `state_schema_version:` key in `service.yml`: a service whose highest step is `007_…` is at version 7, and adding `008_…` puts it at 8. See the [ADR-007 amendment](adr/0007-versioning-git-ref.md#amendment-state_schema_version-leaves-the-manifest-2026-09-01-nim-735), which retires the manifest key as an exception to "an artifact's version is never in the manifest". Writing the key anyway is `unknown_key`.

Because the version is derived, the **ladder itself** is what has to be right, and `soul-lint validate-service` reads it: a gap in the numbers (`migration_chain_broken`, one diagnostic per missing version), a repeated number (`migration_step_duplicate`), a `001_…` step (`migration_step_number_invalid` — version 1 is the empty ladder), an entry it cannot read as a step — a directory whose name is not `<NNN>_<slug>`, or a `<NNN>_…` that is not a directory at all (`migration_step_name_invalid`), the retired flat form (`migration_layout_retired`), a step directory with no `main.yml` (`migration_step_main_missing`), a `migrations/` that cannot be listed because it is not a directory (`migration_ladder_unreadable`), and a step that states its own place (`migration_version_key`, at the line the key is written on). A file that is not named like a step — `schema.lock`, a `README.md` — is not a step and is passed over in silence. It runs the SAME scan the keeper resolves the version with, so the linter and the engine cannot disagree about what the ladder says. See [`docs/service/manifest.md` → Validation](service/manifest.md#validation-soul-lint-validate-service).

★ **The engine does not refuse a malformed ladder, and that is deliberate.** A snapshot still has to answer "what version is this" for a repository someone has already broken, and the honest answer is the top of what is on disk; refusing at load would turn the documented preview answer for a broken chain (`reachable: false`, [ADR-0068](adr/0068-service-upgrade-v2.md) §6) into a bad-gateway that says nothing. The strict reading is the linter's, offline, before the repository is pushed.

Deleting a step therefore has three different answers, and which one you get depends on where the deletion is **and** where the incarnation is.

Write `T` for the top of the ladder before the deletion.

| Deleted | The incarnation | What happens |
|---|---|---|
| A step **below the top** | anywhere below the gap | The chain still asks for the missing version: `migration_chain_broken`, the upgrade is refused as unreachable. `soul-lint` reports it offline, one diagnostic per missing version. |
| The **top** step | at `T` | The derived version is now `T-1`, below the incarnation's, so the transition reads as a **downgrade** and is refused (`ErrDowngradeViaRef`, forward-only). Loud, at the moment of upgrade. |
| The **top** step | at `T-1` — the NEW top | ⚠ **Nothing.** `target == current`, the chain is empty, and the upgrade completes as a plain ref-bump. `upgrade-paths` reports `direction: same-schema, reachable: true`, and `soul-lint` is silent — a shorter gapless ladder is a legitimate ladder, so there is nothing offline to complain about either. |
| The **top** step | below `T-1` | A normal forward migration up to `T-1`. The chain is complete and runs; nothing is skipped and nothing is wrong. |

Only the **third** row is a hole, and it is narrower than it looks: the incarnation's state really is at `T-1` and the ladder really does top out at `T-1`, so the two agree. What no longer agrees is `state_schema`, which still describes the shape the deleted step produced — the service's declaration says `T`, its ladder says `T-1`, and nothing in this document reconciles the two. That is the same "described twice, by hand, with nothing comparing them" gap [`schema.lock`](#the-schema-lock) exists for, and the stamped `version` is what catches it: it remembers where the ladder used to end, so a removal is refused on the service repo's own `make validate` before the ref is ever pushed. Until that ships (NIM-737), removing the top step of a published ladder is a change only review catches.

The **column** `incarnation.state_schema_version` is a different thing and is unchanged: it records which version a given live incarnation currently sits on, which is a fact about a row in Postgres, not a declaration in a git repository. The same holds for the API/MCP field of that name.

## The schema lock

`schema.lock` is generated next to the ladder: `version` (the top of the ladder at stamp time) and `fingerprint` (a hash of the **parsed and canonicalized** `state_schema`, never of the file text — a comment would break a text hash). Written by `make schema-stamp` and read by `soul-lint` on the **service repo's** own `make validate` (wherever that repo runs `soul-lint validate-service`) — both targets live in the service repository, the tree holding `service.yml` and `migrations/`, not in this core repo.

It exists because a service describes the shape of `incarnation.state` **twice** — declaratively in `state_schema` ([`docs/service/manifest.md`](service/manifest.md)) and imperatively in this ladder — both by hand, with nothing reconciling the two. Nothing reconciles them offline, and nothing reconciles them inside the upgrade transaction either (see [Atomicity](#atomicity) below). A schema edit that no step implements is therefore invisible until an incarnation is upgraded and its state comes out in a shape the schema does not describe.

What catches what:

| Failure | What catches it |
|---|---|
| A wrong version number on a step | Impossible — the number exists in exactly one place, the directory name. |
| `state_schema` changed with no step added | `schema.lock` — the stamped fingerprint no longer matches the parsed schema. |
| A step that leads to the wrong shape | The step's own tests under `tests/` (see [Testing](#testing)). |

**The fingerprint does not prove the step is correct, only that a step was added.**

**The stamp bypass is procedural, not mechanical.** `make schema-stamp` re-stamps whenever it is run, so an author can edit `state_schema` and re-stamp without adding a step. That is deliberate, and it is how `atlas migrate hash` behaves in Atlas: the bypass shows up in the diff — `schema.lock` changed, no new directory under `migrations/` — and the reviewer reads it there. A mechanical ban (refusing to stamp until the top of the ladder moves) was considered and rejected, because it would force an empty migration step for every harmless schema edit.

> **Doc ahead of code (NIM-737).** This section is the target, not a description of the current tree: nothing writes or reads `schema.lock` yet, and `make schema-stamp` does not exist yet.

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

To iterate through the map, the native macro `.map()` **above the map itself** is used: it bypasses the **keys** (iteration element = key), the value is obtained by the index `m[k]`. This is how `map → array` is collapsed without `keys()` - see migration [`examples/service/redis/migrations/006_acl_users_map_to_array/main.yml`](../examples/service/redis/migrations/006_acl_users_map_to_array/main.yml) (`state.redis_users.map(n, {'name': n, 'perms': state.redis_users[n].perms, ...})`).

### Not allowed in migration-CEL

| Name | Why |
|---|---|
| `vault(...)` | Migration should not involve secrets. |
| `now()` | For test reproducibility. |
| `register.*` | There is no host context (migration - keeper-side). |
| `soulprint.*` | Likewise. |
| `vars.*` | Migration should be a pure function of the old state, not dependent on the service's current defaults. |
| `input.*` | Migration does not accept operator-parameters (only `state`). |
| Any user-defined CEL functions | Sandbox by design. |

## Reverse / downgrade

In MVP - **forward only**. Incident recovery is via `state_history` snapshot (see [`docs/architecture.md → state_history`](architecture.md#state_history--state-change-log)).

Optional `down:` block in the migration file can be added post-MVP without breaking change. The current grammar does not support this block.

## Atomicity

The migration chain from the incarnation's current version to the target version keeper executes in **one PG transaction**:

1. `BEGIN`.
2. `SELECT state, state_schema_version FROM incarnation WHERE name = ? FOR UPDATE`.
3. Apply migrations sequentially in memory (Go): `state_v1 → state_v2 → state_v3 → ...`.
4. At each step `INSERT INTO state_history (state_before, state_after, scenario, changed_by_aid, ...)` with `scenario: "migration"`.
5. `UPDATE incarnation SET state = ?, state_schema_version = ?, service_version = ?`.
6. `COMMIT`.

If any step fails - `ROLLBACK`, the incarnation is marked `status: migration_failed` ([architecture.md → §"Versioning and migration state_schema"](architecture.md#versioning-and-state_schema-migrations)).

**The transaction performs no reconciliation of the post-migration state against the target `state_schema`.** It applies the chain, writes a `state_history` snapshot per step and updates the row ([`keeper/internal/incarnation/crud.go:1791-1833`](../keeper/internal/incarnation/crud.go)); the target schema is never loaded into the transaction, and the only version check it makes is arithmetic on integers — the chain's endpoints against the current and target version numbers. A step that leaves state in a shape the new `state_schema` does not describe commits exactly like a correct one. This is the gap [`schema.lock`](#the-schema-lock) covers offline, and it covers only half of it: the lock catches a schema edited with no step behind it, not a step that produces the wrong shape.

## Secrets and migrations

A migration brings an incarnation up to the new schema, but **not to new secrets**. The migration DSL is a pure function of state and cannot reach Vault ([ADR-019](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) sandbox). A step that re-points the path a secret is derived under therefore leaves the incarnation in one of two wrong states, and **which one depends on the route, not on the verb**: a day-2 step re-resolving an already-stored record **fails closed, loudly** — `config.StripDeclaredSecrets` ([`shared/config/secret_field.go:533-547`](../shared/config/secret_field.go)) removes the secret marker on the way into state, so the stored value carries no mint intent ([`keeper/internal/coremod/state/state.go:597-600`](../keeper/internal/coremod/state/state.go)) and the resolve refuses ([`state.go:679-681`](../keeper/internal/coremod/state/state.go), and [`state.go:682`](../keeper/internal/coremod/state/state.go) for a field with no value and no marker); a create-class step carrying `generate_secret({…})` **silently mints a fresh password at the new path** that the running service does not know ([`state.go:207-216`](../keeper/internal/coremod/state/state.go) pre-read → [`state.go:684-689`](../keeper/internal/coremod/state/state.go)). `core.state.present` guards neither: its semantics are "an existing value is kept, a missing one is minted", not "verify the secret is reachable". **That is why a step that changes a derived secret path must carry a plain-language item: hand the new passwords to consumers after the first run.** For the **shape** of such a change see [`examples/service/redis/migrations/015_system_acl_users/main.yml`](../examples/service/redis/migrations/015_system_acl_users/main.yml) — v14 minted under `secret/redis/<inc>/users/<name>`, v15 derives `secret/redis/<inc>/system_acl_users/<name>`. Cite it for the path shape only: its own description comment overstates the outcome of that particular step, and is corrected by NIM-738.

## Testing

Migration tests live in `migrations/<NNN>_<slug>/tests/<case>.yml`, beside the `main.yml` they exercise. Format:

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

Triggered via `soul-trial <service-repo>/migrations/<NNN>_<slug>/` ([ADR-023](adr/0023-trial-test-runner.md): "executes → soul-trial", as opposed to the purely static `soul-lint`). The runner mechanics are a separate task after the spec.

## Related Documents

- [ADR-019 in `docs/architecture.md`](adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) - committing the solution; the [2026-09-01 amendment](adr/0019-state-migration-dsl.md#amendment-a-step-states-its-place-once-2026-09-01-nim-735) is the one that fixes the layout above.
- [ADR-007](adr/0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field) (amended) - version = git ref; its [2026-09-01 amendment](adr/0007-versioning-git-ref.md#amendment-state_schema_version-leaves-the-manifest-2026-09-01-nim-735) retires `state_schema_version:` from `service.yml`.
- [ADR-009 in `docs/architecture.md`](adr/0009-scenario-dsl.md) - old mention of flat DSL (now replaced by ADR-019).
- [ADR-010 in `docs/architecture.md`](adr/0010-templating.md) - CEL as a single expression engine.
- [`docs/architecture.md` → §"Versioning and migrations state_schema"](architecture.md#versioning-and-state_schema-migrations) - high-level description (`state_schema_version`, upgrade mechanism, atomicity).
- [`docs/architecture.md` → §"`state_history`"](architecture.md#state_history--state-change-log) - a log through which recovery in the event of an incident is available.
- [`docs/templating.md`](templating.md) — CEL general spec.
- [`examples/service/redis/migrations/`](../examples/service/redis/migrations/) - the worked ladder (the first step turns `redis_users` from a list of names into a map `name → {perms, state}` via `foreach`).
