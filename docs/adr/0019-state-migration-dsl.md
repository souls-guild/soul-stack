# ADR-019. State_schema migration DSL

- **Context.** [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) mentions a "flat DSL: `rename`/`set`/`delete`/`move`" as the format of `migrations/<NNN>_to_<MMM>.yml`. But the previous redis-service migration-file example used `{% for %}` (a Jinja2 style from the era before ADR-010) — which **doesn't fit into a flat DSL**. This example was deliberately left untouched during the mass migration under ADR-010 (marked "out of scope, open Q No. 18"). Real state_schema-migration scenarios include collection transforms, computations from old fields, structure splits/merges — a flat DSL isn't enough.
- **Decision.**

  **(a) DSL grammar — flat + CEL expressions + a structural `foreach` (MVP).**

  Operations (a closed list in the MVP): **`rename`** (a move without renaming the location), **`set`** (writing a value; `value:` can be a YAML literal or a CEL expression via `${ … }`), **`delete`** (removal by `path:`), **`move`** (an alias for `rename`), **`foreach`** (a structural loop: `in: <CEL-list/map>`, `as: <var>`, `do: [<operation>, ...]`).

  A conditional `if:` key — **not in the MVP** (the recommended target (c) per the exploration). Extending to (c) — with no breaking change, via adding an optional key.

  The full grammar, test convention, examples — in [`docs/migrations.md`](../migrations.md).

  **(b) CEL — the unified expression engine (like all of Soul Stack per [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)).** In the migration-CEL context:
  - **Available:** `state.*` (the current mutable version), `<as-name>` inside `foreach.do[*]`, standard CEL functions (`int`/`string`/`size`/`has`/`keys`/`values`, comprehensions `map`/`filter`/`all`/`exists`).
  - **Forbidden:** `vault(...)` (don't pull secrets), `now()` (test reproducibility), `register.*` / `soulprint.*` / `essence.*` / `input.*` (a migration is a pure function of the old state, with no host context and no operator parameters), user-defined CEL functions.

  This closes off the migration's execution surface: a side-effect-free `state→state` pass in a CEL sandbox.

  **(c) Atomicity — one PG transaction for the entire migration chain.** On `keeper.incarnation.upgrade name=X to_version=v3.0` (with `state_schema_version: 1` → `3`), keeper:
  1. `BEGIN`.
  2. `SELECT state, state_schema_version FROM incarnation WHERE name = ? FOR UPDATE`.
  3. Apply `001_to_002` → `002_to_003` sequentially in-memory (in-Go).
  4. At every step, `INSERT INTO state_history` with `scenario: "migration"`, `state_before` / `state_after`, `changed_by_aid`.
  5. `UPDATE incarnation SET state, state_schema_version, service_version`.
  6. `COMMIT`.

  On failure — `ROLLBACK`, `incarnation.status: migration_failed` ([§"Versioning and state_schema migrations"](../architecture.md#versioning-and-migrations-state_schema)).

  The final status on a successful upgrade is **`drift`, not `ready`** (see the [amendment below](#amendment-upgrade--drift-the-final-status-2026-06-27); the migration changes the DB state, but not the hosts' rollout).

  **(d) Reverse — forward-only in the MVP.** A `down:` block is not supported. Recovery in an incident goes through a `state_history` snapshot. Extending to an optional `down:` post-MVP — with no breaking change (a new optional top-level file key).

  **(e) An escape module (`state.migrate` / `core.incarnation.state-migrate`) — not introduced in the MVP.** The old reference in [§"Versioning and state_schema migrations"](../architecture.md#versioning-and-migrations-state_schema) to a "destiny module `state.migrate`" is rejected: the name is outside the dictionary (not in [naming-rules.md](../naming-rules.md)), and the real complex cases (which per the exploration make up <10%) are covered by grammar (a). If it's ever needed — a separate ADR with a propose-and-wait on the name (`core.incarnation.state-migrate` — a candidate modeled on `core.soul.registered`).

  **(f) Migration tests — in `migrations/<NNN_to_MMM>/tests/<case>.yml`.** Format: `state_before` → migration → assert `state_after`. Symmetric to the destiny/scenario convention (tests next to the artifact under test). The full format — in [`docs/migrations.md`](../migrations.md).

  **(g) Relation to ADR-009 and ADR-010.** This ADR is an explicit **extension** of the "flat DSL" from ADR-009 into grammar (a). ADR-009, for the migration-DSL part, refers here. Using CEL — consistent with ADR-010 (one expression engine across all of Soul Stack).

- **Consequences.**
  - `docs/migrations.md` — a new file (the normative format spec).
  - `docs/architecture.md` § "Versioning and state_schema migrations" — updated with a reference to ADR-019 (the old "flat DSL" description → "per ADR-019").
  - The redis-service migration-file example is rewritten for grammar (a) (a structural `foreach` instead of `{% for %}` Jinja). Implemented in [`examples/service/redis/migrations/001_to_002.yml`](../../examples/service/redis/migrations/001_to_002.yml) after the redis consolidation (`redis_users` from a list of names to a map `name → {perms, state}`).
  - Open Q No. 18 is closed.
  - Soul-side isolation: a migration is keeper-side, no changes to `proto/keeper/v1/`.
- **Trade-offs.**
  - The grammar is a bit wider than a flat DSL — `foreach` needs specifying (one new key). This is offset by symmetry with the essence pipeline (`foreach: + as: + when:` are already fixed) — the operator recognizes the pattern.
  - The `if` key is deferred — conditional record migrations are done via `foreach + filter` in CEL (`in: ${ state.users.filter(u, u.flag) }`). Less obvious than an explicit `if` in the DSL, but covers the cases. Extending to (c) — on first request.
  - Forward-only — the operator cannot declaratively roll back a migration. Accepted: recovery via `state_history` is a working path, a mandatory `down:` is overkill for a rare operation.

### Amendment: upgrade → drift (the final status, 2026-06-27)

On a successful `keeper.incarnation.upgrade` (step 5 in (c)), the final `UPDATE incarnation` sets **`status = drift`, not `ready`**. Reason: the upgrade transaction migrated `incarnation.state` + changed `state_schema_version`/`service_version` in the DB, but **the hosts stayed on the old rollout** — the real state diverges from the new state, and without a signal, the state↔fact desync would accumulate silently until the next apply. `drift` here is the same informational, non-blocking status as Scry's ([ADR-031(d)](0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): a signal to the operator "roll the new service version onto the hosts"; remediation is a **normal apply** (`drift → ready`), no separate command is needed. The transition is recorded by a separate zero-diff `state_history` entry with `scenario: upgrade-pending-apply` (after the migration's step snapshots; `state_before == state_after` = the post-migration state), so triage can distinguish upgrade-drift from drift found by the Scry scan. The upgrade-tx gate only lets the final `UPDATE` through from `ready`/`drift` (`applying → Busy`, `error_locked`/`migration_failed → Locked` are not overwritten). Implemented in [`keeper/internal/incarnation/crud.go`](../../keeper/internal/incarnation/crud.go) (`upgradeTx`, `writeUpgradeDriftHistory`, `upgradeDriftScenarioLabel`).
