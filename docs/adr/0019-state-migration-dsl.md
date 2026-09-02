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

  On failure — `ROLLBACK`, `incarnation.status: migration_failed` ([§"Versioning and state_schema migrations"](../architecture.md#versioning-and-state_schema-migrations)).

  The final status on a successful upgrade is **`drift`, not `ready`** (see the [amendment below](#amendment-upgrade--drift-the-final-status-2026-06-27); the migration changes the DB state, but not the hosts' rollout).

  **(d) Reverse — forward-only in the MVP.** A `down:` block is not supported. Recovery in an incident goes through a `state_history` snapshot. Extending to an optional `down:` post-MVP — with no breaking change (a new optional top-level file key).

  **(e) An escape module (`state.migrate` / `core.incarnation.state-migrate`) — not introduced in the MVP.** The old reference in [§"Versioning and state_schema migrations"](../architecture.md#versioning-and-state_schema-migrations) to a "destiny module `state.migrate`" is rejected: the name is outside the dictionary (not in [naming-rules.md](../naming-rules.md)), and the real complex cases (which per the exploration make up <10%) are covered by grammar (a). If it's ever needed — a separate ADR with a propose-and-wait on the name (`core.incarnation.state-migrate` — a candidate modeled on `core.soul.registered`).

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

### Amendment: a step states its place once (2026-09-01, NIM-735)

**Amends:** [ADR-007](0007-versioning-git-ref.md) (the `state_schema_version` exception in the Decision list at [`0007:10-11`](0007-versioning-git-ref.md) — the manifest key is retired, and with it the exception), [ADR-0068](0068-service-upgrade-v2.md) (the forward-only symmetry argument at [`0068:47`](0068-service-upgrade-v2.md), which reasons *from* the old notation; the "migrate" disambiguation entry (b) at [`0068:103`](0068-service-upgrade-v2.md); the test-convention consequence at [`0068:113`](0068-service-upgrade-v2.md)), [ADR-023](0023-trial-test-runner.md) (the reference test-format path at [`0023:3`](0023-trial-test-runner.md) — notation only)
**Status:** accepted, **not implemented** (epic NIM-734; this amendment is NIM-735). The **engine** — the directory layout, the version derived from the ladder, and dropping the manifest key — is **NIM-736**, and the derived-version edge case named in the Consequences belongs to it because it is that engine's own behaviour; `schema.lock`, `make schema-stamp` and the `soul-lint` lock check are **NIM-737**; the `examples/` relocation is **NIM-738**.

A step's place in the ladder is stated **exactly once**. Today the same number is written five times: twice in the file name `migrations/001_to_002.yml`, twice in that file's header (`from_version:` / `to_version:`), and once more as `state_schema_version:` in `service.yml`. Five hand-written records of one integer are five chances for it to disagree with itself. The model here is Rails/Flyway: the directory states where the step stands, the file states only what the step does.

**1. Layout.**

A migration step is a directory `migrations/<NNN>_<slug>/` holding `main.yml` and its `tests/`. The number is the version the step leads to; the "from" is derived — the ladder is forward-only and goes by one.

```
migrations/
  002_split_system_and_operator_users/
    main.yml          # description + transform; no from_version, no to_version
    tests/
      splits-system-and-operator-users.yml
      idempotent-on-repeat.yml
  schema.lock         # GENERATED
```

This supersedes the flat form named in Context `:3` and in clause (f) `:36` — a step file `migrations/<NNN>_to_<MMM>.yml` sitting beside a separate `migrations/<NNN>_to_<MMM>/tests/` directory.

★ **The new shape is what finally makes clause (f)'s own claim true.** (f) at `:36` already calls the tests convention "symmetric to the destiny/scenario convention", but under the flat form the artifact is a **file** (`001_to_002.yml`) standing beside a tests **directory** (`001_to_002/`) — which is not the `scenario/` shape at all, and never was. `<NNN>_<slug>/main.yml` + `tests/` is the first time the claim reads literally. The symmetry it appeals to is real and already load-bearing: `scenario/<name>/main.yml` ([`keeper/internal/scenario/scenario.go:52`](../../keeper/internal/scenario/scenario.go)) and `upgrade/<slug>/main.yml` ([`scenario.go:57`](../../keeper/internal/scenario/scenario.go)) share one rule, and the loader constant that spells `main.yml` for both lives at [`keeper/internal/artifact/scenarios.go:31`](../../keeper/internal/artifact/scenarios.go).

**2. `from_version:` and `to_version:` leave the file.** `main.yml` carries `description:` and `transform:`, nothing about its own position. "The ladder goes by one" is **not a new invariant** introduced here: `to == from+1` is already enforced today at [`keeper/internal/statemigrate/parse.go:35-36`](../../keeper/internal/statemigrate/parse.go), and this amendment merely single-sources the number the check was defending.

**3. The version is computed, not stored.**

The state-schema version is not stored anywhere — it is the top of the ladder. An empty `migrations/` means version 1.

`state_schema_version:` therefore leaves `service.yml`. That is an edit to [ADR-007](0007-versioning-git-ref.md), where the key is named as an explicit exception to "an artifact's version is never in the manifest": the exception is retired rather than reworded, which restores ADR-007's own one-source-of-truth principle to the state schema instead of carving around it.

**4. `schema.lock` — because the structure is described twice.**

`schema.lock` is generated next to the ladder: `version` (the top of the ladder at stamp time) and `fingerprint` (a hash of the **parsed and canonicalized** `state_schema`, never of the file text — a comment would break a text hash). Written by `make schema-stamp` and read by `soul-lint` on the **service repo's** own `make validate` (wherever that repo runs `soul-lint validate-service`) — both targets live in the service repository, the tree holding `service.yml` and `migrations/`, not in this core repo.

It exists because a service describes the shape of `incarnation.state` **twice** — declaratively in `state_schema`, imperatively in the migration ladder — both by hand, with nothing reconciling them. Nothing does so offline, and nothing does so inside the upgrade transaction either:

- the upgrade transaction never loads the target schema. `upgradeTx` ([`keeper/internal/incarnation/crud.go:1731-1856`](../../keeper/internal/incarnation/crud.go)) receives an `UpgradeInput` ([`crud.go:1613-1635`](../../keeper/internal/incarnation/crud.go)) carrying `TargetSchemaVer int` and `Chain` — no schema travels with it — and its chain sanity check ([`crud.go:1770-1784`](../../keeper/internal/incarnation/crud.go)) is arithmetic on integers only: chain endpoints against the current and target version numbers. The migrated state is written ([`crud.go:1791-1833`](../../keeper/internal/incarnation/crud.go)) without ever being compared to the shape it was migrated toward.
- the Apply-side check on a `core.state.*` capture is a bare "is the field declared" lookup ([`keeper/internal/coremod/state/state.go:175-177`](../../keeper/internal/coremod/state/state.go), `topLevelProperty` at [`state.go:756-760`](../../keeper/internal/coremod/state/state.go)) — presence of the top-level key, with no type, no `required`, no `items`.

So a schema edit that no step implements, or a step whose result the schema no longer describes, is today caught by nothing at all. The fingerprint closes the first half offline; the second half stays open, and it is the same gap as the still-open question on strict `incarnation.state`-versus-`state_schema` validation in [`docs/service/manifest.md`](../service/manifest.md) — this amendment does not close it.

**5. What catches what.**

| Failure | What catches it |
|---|---|
| A wrong version number on a step | Impossible — the number exists in exactly one place. |
| `state_schema` changed with no step added | `schema.lock` — the stamped fingerprint no longer matches the parsed schema. |
| A step that leads to the wrong shape | The migration's own tests under `tests/`. |

**The fingerprint does not prove the step is correct, only that a step was added.** It is a "you changed the schema, say so in the ladder" gate, not a verification of what the ladder does.

**6. The stamp bypass is closed procedurally, not mechanically.**

`make schema-stamp` re-stamps whenever it is run, so an author can edit `state_schema` and re-stamp without adding a step. That is deliberate, and it is exactly how `atlas migrate hash` behaves in Atlas: the bypass is visible in the diff — `schema.lock` changed, no new directory under `migrations/` — and a reviewer reads it there. The mechanical ban (the stamp refuses until the top of the ladder moves) was **rejected**, because it would force an empty migration step for every harmless schema edit, and a ladder padded with no-op directories costs more than a two-line diff a reviewer can see.

**7. A migration brings an incarnation up to the new schema, but not to new secrets.**

The migration DSL is a pure function of state and cannot reach Vault (the (b) sandbox above). A step that re-points the path a secret is derived under therefore leaves the incarnation in one of two wrong states, and **which one depends on the route, not on the verb**: a day-2 step re-resolving an already-stored record **fails closed, loudly** — `config.StripDeclaredSecrets` ([`shared/config/secret_field.go:533-547`](../../shared/config/secret_field.go)) removes the secret marker on the way into state, so the stored value carries no mint intent ([`keeper/internal/coremod/state/state.go:597-600`](../../keeper/internal/coremod/state/state.go)) and the resolve refuses ([`state.go:679-681`](../../keeper/internal/coremod/state/state.go), and [`state.go:682`](../../keeper/internal/coremod/state/state.go) for a field with no value and no marker); a create-class step carrying `generate_secret({…})` **silently mints a fresh password at the new path** that the running service does not know ([`state.go:207-216`](../../keeper/internal/coremod/state/state.go) pre-read → [`state.go:684-689`](../../keeper/internal/coremod/state/state.go)). `core.state.present` guards neither: its semantics are "an existing value is kept, a missing one is minted", not "verify the secret is reachable". That is why a step that changes a derived secret path must carry a plain-language item: hand the new passwords to consumers after the first run. For the **shape** of such a change see [`examples/service/redis/migrations/014_to_015.yml`](../../examples/service/redis/migrations/014_to_015.yml) — v14 minted under `secret/redis/<inc>/users/<name>`, v15 derives `secret/redis/<inc>/system_acl_users/<name>`. Cite it for the path shape only: its own description comment overstates the outcome of that particular step, and is corrected by NIM-738.

**Consequences of the amendment.**

- The diag codes `migration_version_missing` and `migration_version_invalid` retire — there is no header left to be missing or inconsistent ([`keeper/internal/statemigrate/errors.go:20-21`](../../keeper/internal/statemigrate/errors.go)).
- The derived number is **arithmetically identical** to today's for every service that satisfies the documented completeness rule (version N ⟺ a gapless chain 1→…→N), so [ADR-0068](0068-service-upgrade-v2.md)'s `upgrade-paths` keeps working unchanged — only the source of the target integer moves ([`keeper/internal/incarnation/upgrade_prepare.go:102`](../../keeper/internal/incarnation/upgrade_prepare.go), [`keeper/internal/api/handlers/incarnation_upgrade_paths.go:167`](../../keeper/internal/api/handlers/incarnation_upgrade_paths.go)). The runtime column `incarnation.state_schema_version` is untouched.
- An open question for NIM-736: today deleting a migration file yields `ErrMigrationChainBroken` ([`upgrade_prepare.go:121-124`](../../keeper/internal/incarnation/upgrade_prepare.go), surfaced to the operator as `reachable: false`). Under a derived version, deleting the **top** step silently lowers the target instead — the transition then reads as a no-op or a downgrade rather than as a broken chain, which is a different answer to the same mistake.
