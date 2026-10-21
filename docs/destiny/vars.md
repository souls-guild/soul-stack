# `vars.yml` - destiny locals

The file `vars.yml` next to `destiny.yml` declares **destiny local variables** - static values that the author of destiny has nailed down and cannot be overridden from the outside. Available in tasks as `${ vars.<name> }` (in string interpolation; in top-level expression-keys of type `when:` - naked `vars.<name>`, see [`docs/templating.md`](../templating.md)).

## Why

Without `vars:`, destiny has two extremes:

- **Hardcode** paths/names/prefixes directly into tasks (`params: { path: "/etc/redis/redis.conf", ... }`) - copy-paste for all tasks, hell when changed.
- **Pass** them through `input:` - but then they are included in the **external contract** and someone outside can (and therefore will definitely do) redefine them. And the author meant that this is an invariant of destiny, and not a point of variability.

`vars:` - the third way: the variables are there, it's convenient to use, but they are not visible from the outside and cannot be distorted.

## Semantics

- **Source of truth - `destiny-<name>/vars.yml`** (next to `destiny.yml`).
- **Isolated in destiny.** Neither the scenario, nor the operator via the API, nor the service's own `vars/` **override** these values. If the operator needs the ability to substitute, the corresponding value should be in `input:`, not in `vars:`. Inside the destiny, a task's own `vars:` may override a name — see [one view per task](#one-view-per-task-task-level-vars-over-varsyml).
- **Can refer to `input.*`.** Vars are calculated **after** input validation - that is, the expression `"/etc/redis/users/${ input.user }.acl"` is valid. The reverse (input refers to vars) is not: input comes from outside, before vars even exist.
- **Available in all tasks** of the same destiny - in `tasks/main.yml` and in any neighbor connected via `include:`.
- **Evaluated when read, not up front.** A var is evaluated the first time something reads it, once, and a var nothing reads is never evaluated ([ADR-0082 amendment 2026-10-15](../adr/0082-service-vars.md#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late)) — see [How a var is evaluated](#how-a-var-is-evaluated).

## File format

Top-level YAML-map. No wrapper (`vars:` with a top-level key inside the file - tautological, the path to the file already informs the context).

```yaml
# redis/vars.yml
redis_unit_name: redis-server
redis_conf_path: /etc/redis/redis.conf
redis_data_dir:  /var/lib/redis
redis_user:      redis
redis_group:     redis

# Reference to input.* is allowed
acl_file_path: "/etc/redis/users/${ input.user }.acl"
```

Valid value types: the same as in `input:` ([docs/input.md](../input.md)) - string / integer / number / boolean / array / object. Template expressions `"${ … }"` ([ADR-010](../adr/0010-templating.md), [docs/templating.md](../templating.md)) are interpolated in **every string, at any depth** — a string inside a map or a list is interpolated like a top-level one. The cell rule is the usual one: a cell that is one whole `${ … }` takes the native type of its result, anything else is the stringified concatenation. Non-string scalars (number / bool) pass through literally.

```yaml
base: /etc/redis
conf_path: "${ vars.base }/redis.conf"   # "/etc/redis/redis.conf"

paths:
  conf: "${ vars.base }/redis.conf"      # "/etc/redis/redis.conf" — nested strings are interpolated too
  data: /var/lib/redis
```

## What is NOT in `vars.yml`

- **Externally overridable parameters.** If the operator should be able to override the value, this is the `input:` parameter, not the `vars` local. The border between them is the "contract vs internals" border.
- **Secrets.** Vault links and passwords come through `input:` from `secret: true` (see [input.md](input.md) → section about `secret:`). There should be no secrets in `vars.yml` - it is committed to the git destiny repo.
- **Incarnation-specific values.** Cluster names, master FQDNs, capacity numbers are `input:` because they **change** between incarnations of the same service. `vars:` - about invariants that are the same for all incarnations.

## Use in tasks

```yaml
# redis/tasks/apply.yml
- name: Install redis-server package
  module: core.pkg.installed
  params:
    name: "${ vars.redis_unit_name }"   # "redis-server"
    version: "${ input.version }"       # parameter from caller

- name: Render redis.conf from template
  module: core.file.rendered             # ADR-010: rendering is done by core.file.rendered, not core.file.present + render()
  params:
    path: "${ vars.redis_conf_path }"
    template: templates/redis.conf.tmpl
    vars:
      maxmemory: "${ input.maxmemory }"
    mode: "0640"
    owner: "${ vars.redis_user }"
    group: "${ vars.redis_group }"
```

## `vars` vs `input` - table of differences

| | `input.<name>` | `vars.<name>` |
|---|---|---|
| **Source** | caller (scenario.apply.input or direct API call) | `destiny-<name>/vars.yml` |
| **Who decides the value** | operator/service/test | by destiny |
| **Described in the schema?** | yes, `input:` to `destiny.yml` ([input.md](input.md)) | no, plain map |
| **Validated?** | yes, two rounds (Keeper + Soul) | no (these are the values from the destiny developer himself) |
| **Overridable externally?** | yes - this is its meaning | **no** |
| **Visible in the logs apply?** | yes (parameter values are visible as part of the audit) | yes |
| **Masked (`secret`)?** | yes, via `secret: true` in the schema | no - secrets are not written here |
| **Visible in API response** | like `input:` block | like `vars:` block |

## What is available inside `vars` via templates

In the expressions `"${ … }"` (CEL interpolation, see [ADR-010](../adr/0010-templating.md)) on the right side of `vars.yml` the following is available:

- `input.<name>` - validated destiny parameters.
- `soulprint.self.<name>` - current host facts ([ADR-018](../adr/0018-soulprint-typed.md): `soulprint.self.os.family`, `soulprint.self.network.primary_ip`, `soulprint.self.memory.total_mb`, …).
- `incarnation.*` - the incarnation the destiny is applied for, without its `state` snapshot.
- **`vars.<other>`** - any name of the view of the task that reads it: another `vars.yml` name, or that task's own `vars:` (see [one view per task](#one-view-per-task-task-level-vars-over-varsyml)).

Not available (intentionally):

- **`register.<name>`** - task results. The Keeper renders a whole destiny before any of its tasks runs.
- **The SERVICE's `vars/`** - not visible here. Since [ADR-0082](../adr/0082-service-vars.md) both layers are spelled `vars.*`, but inside a destiny pass the name resolves to this file and the destiny's own task `vars:`, and nothing else; the service decides which of its values to put into the destiny's `input:` when calling it. The name survives the `apply:` boundary, the meaning does not — the same asymmetry that forced `apply_when_dynamic_unsupported` ([orchestration.md §2.1.2](../scenario/orchestration.md)).
- **`soulprint.hosts` / `soulprint.where(...)`** — cross-host scenario-only accessors. In the destiny pass they are cut off by isolation (error on compile). Reading another var does NOT open them: each definition keeps the environment of the file it was written in.

### How a var is evaluated

The normative rule is the [ADR-0082 amendment 2026-10-15](../adr/0082-service-vars.md#amendment-2026-10-15-nim-1014-vars-are-evaluated-on-request-and-bound-late); this is what it means for a `vars.yml` author.

- **On request, once.** `vars` is a lazy map: a var is evaluated the first time a cell reads it and the value is reused. A var nothing reads is never evaluated — a broken one included — and the unit of request is the top-level name (`vars.paths.conf` evaluates all of `paths`). A `vars.yml` value is evaluated at most once per destiny pass per host, and reused by every task whose own `vars:` override none of the names it read.
- **The order of declaration does not matter.**

  ```yaml
  # equivalent in any order of lines
  root_owner: root
  root_group: "${ vars.root_owner }"   # "root"
  ```

- **Every read form reaches the same lookup.** `vars.x`, `vars['x']` and `vars[expr]` all work. `has(vars.x)` and `default(vars.x, y)` EVALUATE `x` and raise its error rather than hiding it; only `'x' in vars` and `size(vars)` look at names without evaluating anything.
- **Cycle** (`a → b → c → a`, including a self-reference with nothing below, `a → a`) → render error `var_cycle`, with the cycle starting at its alphabetically smallest name whatever was read first.
- **A reference to a name nothing defines** is the ordinary CEL no-such-key, raised when the referencing var is read and naming it. It is no longer raised for a var nobody reads.
- **Checked at edit time.** `soul-lint` reports both offline as ERRORs — `var_cycle` and `var_unknown_ref` ([soul-lint.md](../soul-lint.md#var-evaluation-checks)) — and an L0 trial renders in a strict mode that materializes every task view whole, so a broken var fails its case even if no task reads it — as long as the case's render builds a view that holds it.

## One view per task: task-level `vars:` over `vars.yml`

The `vars.*` namespace of a destiny pass has two rungs: `vars.yml` (this document) and task-level `vars:` ([tasks.md §9](tasks.md#vars-task-level)), with a `block:`'s vars merged into each child task's. A task sees ONE view of them:

- **A task-level `vars:` name overrides the `vars.yml` name.** On a task with its own `vars: { redis_unit_name: … }`, `${ vars.redis_unit_name }` is the task value; neighbouring tasks still see the `vars.yml` value.
- **Every definition reads through that view — late binding.** A task var may read a `vars.yml` name, and a `vars.yml` definition read by that task sees the task's override:

  ```yaml
  # vars.yml
  conf_dir: /etc/redis
  acl_file: "${ vars.conf_dir }/users.acl"

  # tasks/main.yml
  - name: Write ACL for the staging layout
    module: core.file.present
    vars:
      conf_dir: /opt/redis
    params:
      path: "${ vars.acl_file }"         # /opt/redis/users.acl
      content: "..."
  ```

  A self-reference reads the rung below: a task's `conf_dir: "${ vars.conf_dir }/staging"` reads `conf_dir` from `vars.yml`.
- **Scope isolation is preserved.** The pass stays isolated from its caller: the service's `vars/` and the scenario's `vars:` are NOT visible (only what `apply: input:` passes), and `register.*`/`soulprint.hosts` stay unavailable to a `vars.yml` value however it is reached.
- **`soul-lint` raises `warn` (`vars_collision`)** for each name declared in both `vars.yml` and task-level `vars:` of the same destiny. This is not a mistake (the override is clear), but almost always an oversight by the author: rename one of the two or rely on the redefinition deliberately.

## See also

- [manifest.md](manifest.md) - layout of the destiny folder, where `vars.yml` is located.
- [input.md](input.md) - external contract destiny.
- [tasks.md](tasks.md) - template task context, where `vars.*` are available.
