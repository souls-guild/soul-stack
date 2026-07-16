# `vars.yml` - destiny locales

The file `vars.yml` next to `destiny.yml` declares **destiny local variables** - static values that the author of destiny has nailed down and cannot be overridden from the outside. Available in tasks as `${ vars.<name> }` (in string interpolation; in top-level expression-keys of type `when:` - naked `vars.<name>`, see [`docs/templating.md`](../templating.md)).

## Why

Without `vars:`, destiny has two extremes:

- **Hardcode** paths/names/prefixes directly into tasks (`params: { path: "/etc/redis/redis.conf", ... }`) - copy-paste for all tasks, hell when changed.
- **Pass** them through `input:` - but then they are included in the **external contract** and someone outside can (and therefore will definitely do) redefine them. And the author meant that this is an invariant of destiny, and not a point of variability.

`vars:` - the third way: the variables are there, it's convenient to use, but they are not visible from the outside and cannot be distorted.

## Semantics

- **Source of truth - `destiny-<name>/vars.yml`** (next to `destiny.yml`).
- **Isolated in destiny.** Neither the scenario, nor the operator via the API, nor the essence service **do** interrupt the values. If the operator needs the ability to substitute, the corresponding value should be in `input:`, not in `vars:`.
- **Can refer to `input.*`.** Vars are calculated **after** input validation - that is, the expression `"/etc/redis/users/${ input.user }.acl"` is valid. The reverse (input refers to vars) is not: input comes from outside, before vars even exist.
- **Available in all tasks** of the same destiny - in `tasks/main.yml` and in any neighbor connected via `include:`.

## File format

Top-level YAML-map. No wrapper (`vars:` with a top-level key inside the file - tautological, the path to the file already informs the context).

```yaml
# redis/vars.yml
redis_unit_name: redis-server
redis_conf_path: /etc/redis/redis.conf
redis_data_dir:  /var/lib/redis
redis_user:      redis
redis_group:     redis

# Link to input is allowed.*
acl_file_path: "/etc/redis/users/${ input.user }.acl"
```

Valid value types: the same as in `input:` ([docs/input.md](../input.md)) - string / integer / number / boolean / array / object. Template expressions `"${ … }"` ([ADR-010](../adr/0010-templating.md), [docs/templating.md](../templating.md)) resolve **only when the entire value of var is a string** (top level value). non-string values ​​(map / list / number / bool) pass **literally through** - CEL does not touch them.

> **Limitation (known): `${ … }` inside map/list values ​​are NOT resolved.** If the var value is map or list, nested `${ … }` within its elements remain **raw text** and are not resolved. Resolve is applied to the entire string value, not recursively by structure.
>
> ```yaml
> base: /etc/redis # string - ok
> conf_path: "${ vars.base }/redis.conf" # string - resolves to "/etc/redis/redis.conf"
>
> paths: # map value - NOT resolved
>   conf: "${ vars.base }/redis.conf" # will remain the literal "${ vars.base }/redis.conf"
> ```
>
> If you need to assemble a nested structure from other vars, assemble each line sheet as a separate var, or build the entire map with one `${ … }` expression (entire cell = one marker → native type, see [templating.md](../templating.md)). Recursive rendering based on map/list depth - not implemented.

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
| **Described in the diagram?** | yes, `input:` to `destiny.yml` ([input.md](input.md)) | no, plain map |
| **Validated?** | yes, two rounds (Keeper + Soul) | no (these are the values ​​​​from the destiny developer himself) |
| **Overridable externally?** | yes - this is its meaning | **no** |
| **Visible in the logs apply?** | yes (parameter values ​​are visible as part of the audit) | yes |
| **Masked (`secret`)?** | yes, via `secret: true` in the schema | no - secrets are not written here |
| **Visible in API response** | like `input:` block | like `vars:` block |

## What is available inside `vars` via templates

In the expressions `"${ … }"` (CEL interpolation, see [ADR-010](../adr/0010-templating.md)) on the right side of `vars.yml` the following is available:

- `input.<name>` - validated destiny parameters.
- `soulprint.self.<name>` - current host facts ([ADR-018](../adr/0018-soulprint-typed.md): `soulprint.self.os.family`, `soulprint.self.network.primary_ip`, `soulprint.self.memory.total_mb`, …).
- **`vars.<other>`** is another variable `vars.yml` of the SAME layer (see "var → var" below).

Not available (intentionally):

- **`register.<name>`** - task results. At the time of calculation `vars` there were no tasks yet.
- **`essence.*`** - this namespace does not exist in destiny **at all**. essence - service level concept; service itself decides which values ​​to put into `input:` destiny when called.
- **`soulprint.hosts` / `soulprint.where(...)`** — cross-host scenario-only accessors. In the destiny pass they are cut off by isolation (error on compile). var → var does NOT open them.

### var → var (links inside the layer)

`vars.yml`-variable **may** refer to another variable of the SAME `vars.yml` via `${ vars.<other> }` (ADR-009 / ADR-010 amendment 2026-06-24). Resolve **eager-topological**:

- Dependencies are extracted from CEL-AST (not regex): `${ vars.X }` in value → edge on `X`.
- The layer is resolved in **topological order** - the variable sees the dependencies that have already been calculated. **The order of the declaration in the file does not matter**:

  ```yaml
  # is equivalent in any order of strings
  root_owner: root
  root_group: "${ vars.root_owner }"   # resolves to "root"
  ```

- **Cycle** (`a → b → c → a`, including self-reference `a → a`) → render error `var_cycle` with cycle trace.
- **Reference to a non-existent layer key** (`vars.z`, which does not exist) → render error `var_unknown_ref`. **eager** check: the error is raised even if the referencing var itself is not used anywhere (broken link = typo by the author, not a "deferred" var).
- **Only inside its own layer.** var→var does NOT weaken the isolation: the link is still missing `register.*`/`essence.*`/`soulprint.hosts`. The chain between the file layer and the task layer is not available (see below).
- Index form `vars['key']` is not supported - use select form `vars.key` (the key name must be statically known from the AST).

## Merge file-vars ↔ task-vars (Option A)

The `vars.*` namespace is shared by two sources: file-level `vars.yml` (this document) and task-level `vars:` on a separate task ([tasks.md §9](tasks.md)). When the name is declared in both, **Option A** applies:

- **task-level `vars:` overrides the file-level var of the same name.** File-vars is the base layer, task-vars are placed on top. The outcome is deterministic: on a task with its own `vars: { redis_unit_name: … }`, it is the task value that will end up in `${ vars.redis_unit_name }`, but the file-level will not.
- **var → var works WITHIN each layer, but NOT between layers.** file-var can refer to another **file-var** (eager-topological, see "var → var" above); task-var - to another **task-var** of the same layer. But **interlayer** links are prohibited: file-var does not see task-var, task-var does not see file-var (`${ vars.<foreign_layer> }` gives `var_unknown_ref`). task-vars resolve over the same base context (`input.*` + `soulprint.self.*` + `incarnation.*`) as file-vars, and the file layer is placed under them only AFTER the resolve (override) - so task-var cannot refer to file-var.
- **Scope isolation is preserved.** file-vars resolve inside the destiny pass (after `apply.input` validation), `register.*`/`essence.*`/`soulprint.hosts` are not available to them - just like task-vars destiny-tasks. var→var does NOT weaken the insulation. scenario-level `vars:` in destiny are NOT visible at all (only through `apply: input:`).
- **`soul-lint` raises `warn` (`vars_collision`)** for each name declared in both `vars.yml` and task-level `vars:` of the same destiny. This is not a mistake (Option A is clear), but almost always an oversight by the author: rename one of the two or rely on the redefinition deliberately.

Resolving file-vars is executed **once per destiny-pass** (per-host, because values ​​can refer to `soulprint.self`), and not per task: file-vars are invariant across tasks of the same pass.

## See also

- [manifest.md](manifest.md) - layout of the destiny folder, where `vars.yml` is located.
- [input.md](input.md) - external contract destiny.
- [tasks.md](tasks.md) - template task context, where `vars.*` are available.
