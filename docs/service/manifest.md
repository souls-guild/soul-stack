# Service folder layout and format `service.yml`

This document describes the layout of the service's git repository and the `service.yml` root manifest fields. Related topics are included in separate regulatory documents:

- The concept of Service (what is it in the big picture, the border with destiny / scenario) - [`docs/destiny/concept.md`](../destiny/concept.md) and [`docs/architecture.md → Service`](../architecture.md).
- scenario format is [`docs/scenario/`](../scenario/README.md).
- The standard `input:` block for scenario is [`docs/input.md`](../input.md).
- Migrations state_schema - [`docs/migrations.md`](../migrations.md), [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl).

## What is a service

Service is a **service type** (Redis HA, PostgreSQL, Vector-collector). One service - one git repository with its own manifest, set of operations (scenarios), default parameters (essence), state migrations and tests.

The version of the service as an artifact is the git tag under which the manifest is committed ([ADR-007](../adr/0007-versioning-git-ref.md)). The top-level `version:` field in `service.yml` is intentionally missing.

## Repository layout

```
service-<name>/
├── service.yml                         # manifest (this document)
├── essence/                            # parameters in the hierarchy - see architecture.md → Essence
│   ├── _default.yaml                   # baseline for all incarnation
│   ├── _stack.yaml                     # OPTS: declarative assembly pipeline
│   ├── coven/                          # OPT.: parameters by Coven tags
│   │   ├── prod.yaml
│   │   └── dev.yaml
│   └── os/                             # OPC: parameters by soulprint.os.family
│       ├── ubuntu.yaml
│       └── debian.yaml
├── scenario/                           # operations; auto-discover from directory
│   ├── create/
│   │   ├── main.yml                    # entry point: input + state_changes + tasks
│   │   ├── install.yml                 # OPTS: include-neighbors main.yml
│   │   ├── vars.yml                    # OPTS: scenario-locals
│   │   ├── templates/                  # OPTS: scenario-local templates
│   │   └── tests/                      # OPT: script tests
│   ├── add_user/
│   │   └── main.yml
│   ├── update_acl/
│   │   └── main.yml
│   └── restart/
│       └── main.yml
├── upgrade/                            # OPTs.: version-to-version upgrade scenarios (2nd auto-discovery channel - ADR-0068)
│   └── v2/
│       └── main.yml                    # top-level from: [<source tags>] + host transition tasks
├── types.yml                           # OPTS: reused named input types (section types:, $type-link - ADR-062)
├── migrations/                         # OPTS: migration state_schema (if state_schema_version > 1)
│   ├── 001_to_002.yml
│   ├── 001_to_002/
│   │   └── tests/                      # migration tests (state_before → state_after)
│   └── 002_to_003.yml
└── tests/                              # OPT.: service-level tests (smoke, chaos)
    ├── smoke.yml
    └── chaos.yml
```

Only `service.yml` and at least one script (`scenario/<name>/main.yml`) are required. Everything else appears as needed.

There is no need to list scripts in `service.yml` - keeper finds them with auto-discover in the `scenario/` directory.

The second auto-discovery channel is the `upgrade/<slug>/` directory next to `scenario/`: version-to-version upgrade scripts with self-describing top-level key `from:` (source versions). They are launched by `POST /v1/incarnations/{name}/upgrade`, and are not shown in regular day-2 script lists. Design - [ADR-0068](../adr/0068-service-upgrade-v2.md), names - [naming-rules.md → Upgrade v2](../naming-rules.md).

## `service.yml` - manifest

The root file contains only the service metadata and the contract for the runtime-state structure. Scenarios and destiny tasks live in adjacent files. This is done deliberately: `service.yml` remains short and can be read in one glance during review.

### Fields

| Field | Obligation | Type | Meaning |
|---|---|---|---|
| `name` | yes | string (kebab-case) | Service type name (`redis`, `postgres-ha`). Coincides with the name of the service folder (in `examples/service/<name>/` - a bare name without the prefix `service-`). Regex `^[a-z][a-z0-9-]*$`. |
| `description` | recommended | string | One or two phrases: what kind of service is this? Visible in UI Keeper, MCP directory, output `soul-lint`. |
| `state_schema_version` | yes | integer (≥1) | Structure version `incarnation.state` in Postgres. **NOT** version of the service (this is the git tag by [ADR-007](../adr/0007-versioning-git-ref.md)). Increments explicitly when breaking schema changes; requires appropriate migration to `migrations/`. |
| `state_schema` | yes | JSON Schema object | Structure of `incarnation.state` JSONB fields in Postgres. Format - JSON Schema (`type: object` at root), draft-07 compatible. See "`state_schema` Format" below. |
| `destiny` | yes (if there are dependencies) | array<{name, ref, git?}> | List of destiny dependencies. Each entry: `{ name: <kebab-case>, ref: <git-tag-or-branch> }` + opt. `git: <full-URL>` (override source, see below). Core modules **are not listed** - they are always available ([ADR-009](../adr/0009-scenario-dsl.md)). |
| `modules` | yes (if there are dependencies) | array<{name, ref}> | List of custom modules `{ name: <namespace>.<module>, ref: <git-tag-or-branch> }`. Core modules **not listed** ([ADR-015](../adr/0015-core-modules-mvp.md)). From the Keeper entries **auto-synthesizes** install steps `core.module.installed` into the run plan - see below. |
| `certificate_rotation` | no | object | Enables and configures **auto-rotation** of the incarnation's service TLS certs by the background Reaper ([ADR-017](../adr/0017-keeper-side-core.md)): fields `enable`/`scenario`/`threshold`/`pki_role`. No section (or `enable: false`) → rotation off. Semantics and example — ["`certificate_rotation` Section"](#certificate_rotation-section). |

### What is NOT in `service.yml`

- **`version`** — service version = git tag under which the file is committed ([ADR-007](../adr/0007-versioning-git-ref.md)). Any appearance of `version:` in `service.yml` is a validation error with a hint about ADR-007.
- **`tasks:` / `steps:`** is a destiny/scenario level. `service.yml` does not list tasks.
- **`input:`** is the scenario level (`scenario/<name>/main.yml`). There is no `service.yml` `input:`.
- **`scenarios[]`** - auto-discover scripts are located in the directory. There is no need to list them in the manifest.

### Format `state_schema`

`state_schema` is a JSON Schema document that describes the expected structure of the JSONB field `incarnation.state` in Postgres. At the root there is always `type: object` with a description of the fields.

Standard JSON Schema draft-07 constructs are supported:

- `type` — `object` / `array` / `string` / `integer` / `number` / `boolean`.
- `required` - list of required keys.
- `properties` — field map → diagram.
- `additionalProperties` - bool or nested schema.
- `enum`, `pattern`, `min`/`max`, `items` for arrays.

Keeper validates `incarnation.state` against `state_schema` when creating an incarnation and when upgrading to a new version of schema via migration (see [`docs/migrations.md`](../migrations.md)).

### Format `destiny[]` and `modules[]`

Each record is an object:

| Field | Obligation | Type | Meaning |
|---|---|---|---|
| `name` | yes | string | Name destiny (for `destiny:`) or `<namespace>.<module>` (for `modules:`). |
| `ref` | yes | string | Git ref - tag (`v2.0.0`) or branch (`main`). No semver-range - exact ref ([ADR-007](../adr/0007-versioning-git-ref.md)). |
| `git` | no | string (full git-URL) | **`destiny[]` only.** Per-entry override source. When `git:` is specified, Keeper loads destiny directly from this URL, ignoring `default_destiny_source` (keeper.yml). In `modules[]` the field is prohibited - the parser rejects with `unknown_key`. |

`name` format:
- `destiny[].name` - kebab-case single-level name destiny, regex `^[a-z][a-z0-9-]*$`.
- `modules[].name` — strict two-level form `<namespace>.<module>`, regex `^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$`. Symmetrical to `destiny.yml → required_modules[]` (see [`docs/destiny/manifest.md`](../destiny/manifest.md)). Core modules are not listed in `modules:`.

**Hybrid of destiny source** (how Keeper outputs git-URL for dependency):
- entry **without** `git:` → standard path: git-URL = `default_destiny_source` (keeper.yml) with `{name}` substitution;
- entry **with** `git:` → override: git-URL is taken from `git:` directly, no template is used.

`ref` is taken from the record in both cases. The full resolution algorithm is [`docs/keeper/config.md → default_destiny_source`](../keeper/config.md#services--default_destiny_source--default_module_source).

Other field extensions (`enabled`, `optional`, etc.) are a separate propose-and-wait.

### `modules[]` - source of auto-synthesis of install steps (ADR-065)

`modules[]` is not just a dependency declaration for validation and UI. Keeper from each record **synthesizes** Soul-side step `core.module.installed` with `params: {name, ref}` and inserts it into the run plan immediately before the first consumer task of the module (task `module: <ns>.<module>.<state>`; consumer inside `block:` → insertion before the entire block). The dependency is declared once per service - install-boilerplate is not needed in each scenario ([ADR-065 amendment 2026-07-03](../adr/0065-core-module-installed.md)).

- **A module without consumer tasks is not synthesized in the script.**
- **Takeover:** an explicit step `core.module.installed` with the same literal `params.name` disables the synthesis of this name - the operator itself controls the position, `ref` and `when:`.
- `ref` records go into the params of the synthesis step as **pin-verification**: the active Sigil tolerance must be on this ref.
- **MVP limitation:** consumers are defined by `module:` script tasks; a module used only inside destiny (via `apply:`) is not considered a consumer - it requires an explicit install step.

Full mechanics (synthesis points, position, token name, idempotency) - [`docs/keeper/modules.md → Auto-synthesis`](../keeper/modules.md).

### `certificate_rotation` Section

An optional top-level section that enables **auto-rotation** of the incarnation's service TLS certs (Redis server TLS, etc.) by the background Reaper ([ADR-017](../adr/0017-keeper-side-core.md); [Warrant](../naming-rules.md#domain-entities) registry, Reaper rule `rotate_due_certs`). This is about the **service** cert, not the Soul agent's identity cert (that's SoulSeed, rotated separately).

```yaml
# service.yml
certificate_rotation:
  enable: true            # enables service auto-rotation; false/no section → off
  scenario: rotate_tls    # operational rotation scenario (scenario/<name>/ folder)
  threshold: 30d          # desired margin before expiry (CURRENTLY informational, see below)
  pki_role: redis-server  # Vault PKI role for signing this service's certs
```

| Field | Required | Type | Meaning |
|---|---|---|---|
| `enable` | yes | bool | Master switch for the service's auto-rotation. `true` — this service's incarnation certs participate in the `rotate_due_certs` scan. `false` — the section is **inert** (equivalent to its absence). |
| `scenario` | yes (when `enable: true`) | string | Name of the operational rotation scenario (`scenario/<name>/`) that the Reaper spawns on the incarnation when a cert nears expiry. Conventionally `rotate_tls`. |
| `threshold` | no | duration | Desired margin before expiry (`30d`). **Currently informational** — see semantics below. |
| `pki_role` | yes (when `enable: true`) | string | Name of the Vault PKI role that signs this service's certs. Read keeper-side on issuance (`core.cert.issued`) and on rotation (Reaper re-sign). |

**Semantics:**

- **No section = rotation off.** The Reaper skips certs of a service without a `certificate_rotation` section — **no** fallback to hardcoded `rotate_tls`. `enable: false` behaves the same (section inert).
- **`threshold` is currently informational.** The field is parsed and validated, but the effective scan threshold `not_after < NOW()+threshold` is taken from the **global** `keeper.yml::reaper.rules.rotate_due_certs.rotate_threshold` (one scan axis per cluster). Per-service `threshold` (+ essence-override) is a follow-up.
- **"What and how" is here; "whether it's enabled and how cautiously" is in keeper.yml.** The manifest declares *what* is rotated, *how* (`scenario`/`pki_role`), and *with what margin* (`threshold`). The cluster-wide caution controls (`enabled`/`dry_run`/`rotate_jitter`/`max_rotations_per_tick`, default OFF+dry_run) live in `keeper.yml::reaper.rules.rotate_due_certs`. Plus a per-cert flag `auto_rotate` (default `true`) in the Warrant registry. Rotation happens when all three gates are true (`enable` x `auto_rotate` x keeper.yml `enabled`).
- **`scenario`/`pki_role` are not duplicated in `params`.** Their single source is this section: the Reaper takes them on rotation, the keeper-side module `core.cert.issued` takes `pki_role` on issuance. The scenario author does NOT pass the PKI role through a step's `params` (blast-radius: the role comes from the git-reviewed manifest, the PKI-engine mount comes from `keeper.yml::vault.pki_mount`). See [ADR-017 amendment 2026-07-09](../adr/0017-keeper-side-core.md) and [keeper/modules.md → `core.cert`](../keeper/modules.md#corecertregistered--corecertissued).
- **Rotation happens for the whole incarnation at once** (all hosts together), not host by host.

### Example

```yaml
name: redis
state_schema_version: 2
description: Redis (standalone/sentinel/cluster/sentinel_only)

# Structure of incarnation.state in the database
state_schema:
  type: object
  required: [redis_type, redis_config]
  properties:
    redis_type: { type: string, enum: [standalone, sentinel, cluster, sentinel_only] }
    redis_version: { type: string }
    redis_config:
      type: object
      additionalProperties: true
    redis_users:                      # map username → {perms, state}
      type: object
      additionalProperties:
        type: object
        required: [perms, state]
        properties:
          perms: { type: string }
          state: { type: string, enum: [on, off] }
    redis_hosts:
      type: array
      items:
        type: object
        properties:
          sid:  { type: string }
          role: { type: string, enum: [primary, replica, sentinel] }

# Dependency artifacts - ref: git tag/branch (see ADR-007)
destiny:
  - { name: redis, ref: v1.0.0 }      # mode-agnostic brick: install + render redis.conf

# Custom modules needed by scripts (two-level form <namespace>.<module>).
# Keeper synthesizes from the entry the install step core.module.installed before the first
# by the consumer in the run plan (ADR-065) - an explicit step in the script is not needed.
modules:
  - { name: community.redis, ref: v1.0.0 }  # live Redis runtime (CONFIG SET, ACL, cluster, sentinel)
```

Working example with full folder layout - [`examples/service/redis/`](../../examples/service/redis/).

## Scripts

Each folder `scenario/<name>/` is a separate operation on the service (CRUD-style: `create`/`add_user`/`restart`/...). `main.yml` - script entry point: contains inline `input:` (input contract for [`docs/input.md`](../input.md)), `state_changes:` (which fields `incarnation.state` the script will update upon success), `tasks:` (steps).

The full regulatory specification of scenario-DSL is [`docs/scenario/`](../scenario/README.md).

### Starting script - `create: true`

The bootstrap script is the one with which the operator creates a new incarnation (`POST /v1/incarnations`, field `create_scenario`). The script declares this top-level ability with the key `create: true` in its `scenario/<name>/main.yml`:

```yaml
# scenario/create/main.yml
name: create
create: true          # script is valid as a starter script (bootstrap of new incarnation)
input:
  # ...
state_changes:
  # ...
tasks:
  # ...
```

Rules:

- **The declaration is in the script, not in the manifest.** The starting set of the keeper service is output by **auto-discover**: scans `scenario/`, the set includes **exactly** scripts with `create: true`. In `service.yml`, startup scripts are not listed (like any others - keeper finds them in the `scenario/` directory, see "Repository Layout").
- **The name `create` is NOT privileged.** A script with the name `create` is included in the set only if it itself carries `create: true` - just like any other. The magical default `create` is no longer there.
- **Several create scripts are the norm.** The service can offer several starting paths (for example, redis: `create` - from scratch, `create_from_souls` - on ready-made hosts, `migrate_cluster` - with data filled from an external source). The operator selects one by field `create_scenario`; if the service has ≥1 create script, the selection is **required** (empty → `422`, input is validated against the schema of a specific script).
- **Service without `create: true` scripts → bare incarnation.** If no script carries `create: true`, `POST /v1/incarnations` creates a **bare incarnation**: write to `ready` without running and without `apply_id` (`incarnation.created_scenario` = `null`). Further work is done through day-2 operations (`POST /v1/incarnations/{name}/scenarios/{scenario}`). Such a service consists only of day-2 scenarios and does not know how to "raise itself from scratch" with one call - this is a valid pattern.

Choice semantics, three branches of the contract and bare-incarnation on the API side - [`docs/keeper/operator-api/incarnations.md → Startup scenario selection and bare-incarnation`](../keeper/operator-api/incarnations.md).

### When you need neighbors `main.yml`

One `main.yml` copes as long as the script remains visible (~150 lines). If logical subsections are clearly identified inside, we move them to `scenario/<name>/<sub>.yml` and connect them through `include:`. Same as [`docs/destiny/manifest.md -> When tasks/main.yml needs neighbors`](../destiny/manifest.md#when-you-need-neighbors-tasksmainyml).

## Reusable named input types - `types.yml`

The optional service-level file `types.yml` declares **reusable named input schemes** - section `types:` with map `<PascalCase>` → scheme in the same input-DSL ([`docs/input.md`](../input.md)). The script refers to the type with the `$type: <Name>` directive (as an independent field or `items: {$type: <Name>}` for an array) - this way a complex type (for example, user record `{name, perms, state}`) is not duplicated inline in each script. Resolve - service-level (only this service), with mandatory cycle-detection. The full format, resolution, error classes and MVP boundaries are [`docs/input.md → "Reusable named types"`](../input.md), [ADR-062](../adr/0062-input-types.md). The previous unrealized provision `$ref` for an external JSON-Schema file in `schemas/` has been replaced by it.

## Essence

Hierarchical assembly of parameters. `essence/_default.yaml` — baseline for all incarnations; optional subdirectories `coven/<label>.yaml` and `os/<family>.yaml` add overlays based on Coven tags and `soulprint.os.family`. Optional `_stack.yaml` - declarative assembly pipeline (complex conditions and iterations).

Full regulatory spec - [`docs/architecture.md → Essence: assembly pipeline`](../architecture.md). Essence - role-agnostic ([ADR-008](../adr/0008-coven-stable-tags.md)): there is NO stage `role/<Y>.yaml` in the pipeline.

## Migrations state_schema

If `state_schema_version > 1`, the repository must have a directory `migrations/` with the migration chain. Format - flat DSL + CEL expressions + `foreach` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

Full regulatory spec - [`docs/migrations.md`](../migrations.md).

Agreements:

- `migrations/<NNN>_to_<MMM>.yml` - migration from NNN to MMM version.
- `migrations/<NNN>_to_<MMM>/tests/<case>.yml` - migration tests (state_before → migration → assert state_after).
- The chain must be complete: migrations `1→2`, `2→3`, …, `(N-1)→N` - without gaps.
- Forward-only in MVP (`down:` not supported, recovery via `state_history`).

Migration is triggered by an explicit operator operation (`keeper.incarnation.upgrade to_version=N`), not automatically by the apply script.

## Validation `soul-lint validate-service`

`soul-lint validate-service <path>` - static check of root `service.yml` and related files. The MVP checks:

- **`service.yml` manifest:**
  - `name` regex `^[a-z][a-z0-9-]*$`, non-empty.
  - `description` — string (if any).
  - `state_schema_version` — integer ≥1.
  - `state_schema` - valid JSON Schema; `type: object` on the root.
  - `destiny[]` / `modules[]` - each entry has `name` + `ref`, both non-empty. `name` matches kebab-case; for `modules:` - two-level form `<namespace>.<module>`. Opt. `destiny[].git` — source override; in `modules[]` the `git:` field is rejected (`unknown_key`).
  - Unknown top-level keys → `unknown_key` with hint about deprecated (`version` → ADR-007; `tasks`/`steps`/`scenarios` → auto-discover/destiny-level; `input` → scenario-level).

- **Correspondence between `state_schema_version` and `migrations/`:**
  - If `state_schema_version == 1` - the `migrations/` directory is optional.
  - If `state_schema_version > 1` - there must be a complete chain of migrations `1→2`, ..., `(N-1)→N` without gaps.
  - Each `migrations/<NNN>_to_<MMM>.yml` file is validated separately (migration format is [`docs/migrations.md`](../migrations.md)).

Extended checks (cross-file refs: every `apply: destiny: <name>` in scripts references an entry in `service.yml → destiny:`; every `module: <ns>.<mod>.<state>` exists in `modules:` or core modules; etc.) - deferred in M1.5 ([`docs/soul-lint.md`](../soul-lint.md)).

## Open Q

- **Strict validation of `incarnation.state` against `state_schema` when applying script** - a mandatory check before each apply, or only when creating an incarnation/upgrade? Associated with Keeper's runtime pipeline (post-MVP).
- **Additional fields in `destiny[]` / `modules[]`** (`enabled`/`optional`/...) - `destiny[]` already carries opt. `git:` (override source), other extensions via propose-and-wait.
- **Cross-file refs validation** (apply: destiny ⊆ service.yml destiny) - postponed in M1.5.

## See also

- [`docs/destiny/manifest.md`](../destiny/manifest.md) - format `destiny.yml`.
- [`docs/destiny/concept.md`](../destiny/concept.md) - the concept of destiny and the Service layer in the big picture.
- [`docs/scenario/`](../scenario/README.md) — scenario format.
- [`docs/input.md`](../input.md) - `input:` block standard (for scripts).
- [`docs/migrations.md`](../migrations.md) - migration format.
- [`docs/architecture.md → Service`](../architecture.md) - architectural summary.
- [ADR-007](../adr/0007-versioning-git-ref.md) - versioning via git ref.
- [ADR-009](../adr/0009-scenario-dsl.md) - scenario/destiny boundary.
- [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) — Migration DSL.
