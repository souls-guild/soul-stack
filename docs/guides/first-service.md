# Your first end-to-end service

This guide is a bridge between [getting-started.md](../getting-started.md) (where you picked up one Keeper, boarded one Soul and used the ready-made `hello-world`) and exploitation. Here you **build the service yourself from scratch**: write its files, register it in Keeper, create an incarnation and see the result on the host.

This is not a reference spec - it's a step-by-step tutorial. Where a complete grammar is needed (all task fields, all CEL semantics, the entire migration format) - I provide a link to the regulatory document. The guide itself is based on a **real working example**: [`examples/service/hello-world/`](../../examples/service/hello-world/). Each piece of YAML below is a file from there, not fiction.

## 1. Purpose and background

**What we'll build.** Minimal service `hello-world`: one operation `create`, which writes the greeting file `/tmp/soul-stack-hello` on each incarnation host, substituting the text from the operator parameter, and fixes the path to the file in `incarnation.state` (Postgres). This is enough to go through the entire chain: operator parameter → CEL render → apply on the host → state commit.

**What you need before starting** (all from [getting-started.md](../getting-started.md)):

- worker Keeper (`make dev-keeper`, responds to `http://127.0.0.1:8080/healthz`);
- at least one onboarded Soul in the status `connected`, bound to coven `demo`;
- Archon token in environment variable: `TOKEN=$(make dev-jwt)` (or `TOKEN=$(cat /tmp/keeper-dev/archon-alice.jwt)`).

Checking that Soul is in place:

```sh
curl -s http://127.0.0.1:8080/v1/souls -H "Authorization: Bearer $TOKEN"
```

The response must contain a host with `status: connected` and `covens: ["demo"]`.

## 2. Service-repo layout

A service is a git repository of a certain form. One service = one service type (`hello-world`, `redis`, `postgres-ha`) = one repository. The service version is the git-ref (tag or branch) under which the files are committed; field `version:` in manifest **no** ([ADR-007](../adr/0007-versioning-git-ref.md)).

Layout of our `hello-world` (minimum - only `service.yml` and at least one script are required):

```
hello-world/
├── service.yml                     # manifest: name, state-schema version, structure incarnation.state
├── essence/
│   └── _default.yaml               # baseline parameters for all incarnations (background)
└── scenario/
    ├── create/
    │   ├── main.yml                # "create" operation: input + state_changes + tasks
    │   └── tests/
    │       └── greeting-hello/case.yml   # L0 test: checks script rendering without hosts
    └── converge/
        └── main.yml                # desired state for drift-check (check-drift)
```

What is specifically **not** here and why - it will be useful so as not to look for unnecessary things:

- `migrations/` - migrations are only needed for `state_schema_version > 1` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)); We have version `1`.
- `destiny[]` / `modules[]` in `service.yml` - our script uses only **core modules** (`core.file.present`), and they are always available and are not listed in the manifest ([ADR-009](../adr/0009-scenario-dsl.md)).
- `templates/` - `.tmpl` files are needed when content is rendered by a Go template; Our content comes inline via `${ input.greeting }`.

The full layout and manifest format is [docs/service/manifest.md](../service/manifest.md).

## 3. `service.yml` - manifest

The manifesto is short in design: only service metadata and **contract for the runtime-state structure**. There are no tasks - they live in scenarios.

[`examples/service/hello-world/service.yml`](../../examples/service/hello-world/service.yml):

```yaml
name: hello-world
state_schema_version: 1
description: Minimal service with real state-change for E2E (file write + commit to incarnation.state)

state_schema:
  type: object
  properties:
    greeting_file:
      type: string
```

Parsing fields:

- **`name`** is the name of the service type in kebab-case (`^[a-z][a-z0-9-]*$`). Same as the folder name without the `service-` prefix. Keeper resolves the service using this name when creating an incarnation.
- **`state_schema_version`** is the **structure** version of `incarnation.state`, not the service version (version = git-ref). It is incremented only when there is a breaking change in the state structure and then requires migration. We have `1` → the `migrations/` directory is not needed.
- **`description`** - one or two phrases; visible in the Keeper UI, MCP directory and `soul-lint` output.
- **`state_schema`** - JSON Schema (draft-07-compatible, always `type: object` at the root), describing the JSONB field `incarnation.state` in Postgres. Here we declare a single field `greeting_file` of type string. Keeper validates state against this schema when creating an incarnation and when upgrading a schema version.

Full list of manifest fields (including `destiny[]` / `modules[]` for services with dependencies) - [docs/service/manifest.md → `service.yml`](../service/manifest.md).

## 4. `scenario/create/main.yml` - write the operation

A script is one operation on a service (CRUD-style: `create` / `add_user` / `restart` / ...). Each `scenario/<name>/` folder is a separate operation; Keeper finds them with auto-discover; there is no need to list them in the manifest. The entry point is `main.yml` with three blocks: `input:` (input contract), `state_changes:` (what to write in state), `tasks:` (steps).

[`examples/service/hello-world/scenario/create/main.yml`](../../examples/service/hello-world/scenario/create/main.yml):

```yaml
name: create
description: Creates a greeting file on each incarnation host and fixes the path to incarnation.state.

input:
  greeting:
    type: string
    required: true
    description: The text that is written to the greeting file.

state_changes:
  sets:
    greeting_file: /tmp/soul-stack-hello

tasks:
  - name: Write greeting file on every host of the incarnation
    module: core.file.present
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
```

Analysis by blocks.

**`input:` - script parameters.** Contract of what the operator must/can pass at startup. There is one parameter `greeting`: string, required (`required: true`). Keeper validates the passed `input` against this contract **before** the run - if the operator does not pass `greeting`, the run does not start. Full standard `input:` (types, formats, validation, reusable named types via `types:`/`$type`) - [docs/input.md](../input.md).

**`tasks:` - operation steps.** One step:

- `module: core.file.present` is a core module that ensures the existence of a file with the given content (idempotent: if the file is already like this, there is no change). The behavior of the module and all its parameters are [docs/module/core/file/README.md](../module/core/file/README.md). The complete list of core modules is [ADR-015](../adr/0015-core-modules-mvp.md).
- `params.path` - where to write; `params.content` - what to write.
- **`content: "${ input.greeting }"`** - the template engine works here. The `${ … }` marker is a CEL interpolation: during the render phase, Keeper substitutes the value `input.greeting` into the string. That is, the file text comes from the operator parameter, and is not hardwired into the script. The border "where is CEL, where is Go template", marker `${ … }`, security model - normative in [docs/templating.md](../templating.md).

**`state_changes:` - what to record in state after success.** The key `sets` is the map `<state field>: <value>`. After **all** incarnation hosts have successfully completed the cross-host barrier, Keeper writes `incarnation.state.greeting_file = /tmp/soul-stack-hello` to Postgres. Here the value is a literal; in the general case, this is also a CEL expression (you can use `${ input.* }`, `${ register.* }`, etc.). The barrier/state-commit invariant and the grammar `state_changes` (`sets` / future `appends` / `modifies`) - [docs/scenario/orchestration.md → §7](../scenario/orchestration.md).

> Why is `on:` / `where:` not here. `on:` is the target of the step (on which hosts to execute). The omitted `on:` means "entire incarnation"—all **member** hosts (via the membership relation; `incarnation.name` is not a Coven). That's enough for us. Targeting by covens (`on:`) and volatile per-host predicate (`where:`) - [orchestration.md → §3–§4](../scenario/orchestration.md).

### Script test (optional, but useful)

Next to the script is an L0 test - it checks that the **render** gives the expected tasks, without real hosts. [`scenario/create/tests/greeting-hello/case.yml`](../../examples/service/hello-world/scenario/create/tests/greeting-hello/case.yml):

```yaml
name: create writes greeting file with input.greeting

fixtures:
  input:
    greeting: hi

assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
```

Meaning: "at `input.greeting=hi`, exactly one task `core.file.present` with `content: hi` will be rendered." This catches regressions in the render (for example, if someone breaks CEL interpolation). Testing levels - [docs/destiny/testing.md](../destiny/testing.md).

## 5. `essence/_default.yaml` - default parameters

Essence - hierarchically collected incarnation parameters (Salt-pillar analogue). `_default.yaml` — baseline for all incarnations; You can put overlays on top of it using Coven-tags (`essence/coven/<label>.yaml`) and OS-family (`essence/os/<family>.yaml`).

[`examples/service/hello-world/essence/_default.yaml`](../../examples/service/hello-world/essence/_default.yaml):

```yaml
greeting: hello from soul stack
```

In our minimal scenario, `essence.greeting` is a **substrate for the future**: `create` itself requires `input.greeting` to be mandatory, so the text comes from the operator. Essence shows how a service could carry default values ​​without requiring them from the operator every time. Full regulatory specification of the essence assembly pipeline (overlays, `_stack.yaml`) - [docs/architecture.md → Essence](../architecture.md).

> `input:` vs `essence` - what's the difference. `input:` is a contract **for calling a script** (the operator is passed at startup; validated before execution). `essence` are **incarnation parameters** collected from the git substrate + overlays; they are available as context and can be inserted into tasks. They have different life cycles: input - per-run, essence - per-incarnation.

## 6. Offline validation

Before registering a service, run the static linter - it catches structural errors in the manifest and script without running Keeper:

```sh
./soul-lint/bin/soul-lint validate-service  examples/service/hello-world/service.yml
./soul-lint/bin/soul-lint validate-scenario examples/service/hello-world/scenario/create/main.yml
```

Both should give exit 0 and `OK: <path>`. What exactly does the linter check (name regex, JSON Schema at the root, compliance with `state_schema_version` ↔ `migrations/`, forbidden keys) - [docs/service/manifest.md → `soul-lint validate-service`](../service/manifest.md) and [docs/soul-lint.md](../soul-lint.md).

## 7. Register the service

For Keeper to resolve a service, it must be added to the service registry: git source + ref. The version is `ref` (tag or branch), not a separate field ([ADR-007](../adr/0007-versioning-git-ref.md)). In production it is `POST /v1/services`:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/services \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello-world", "git": "https://git.internal/svc/hello-world.git", "ref": "main"}'
```

> **Dev-shortcut.** It is inconvenient to keep a git repo on a local stand. `make dev-provision` already materializes `hello-world` as a local `file://` repo and seeds the service registry - a separate `POST /v1/services` is then not needed. This is dev-only: `file://`-resolve is enabled by the `SOUL_STACK_ALLOW_FILE_REPOS=1` flag, which sets `make dev-keeper`; in production, the source is a real git-URL. More details - [getting-started.md → Step 7](../getting-started.md).

When you edit your service: commit changes, set the required `ref` (move branch or set a new tag) - Keeper will pull up exactly this ref at the next resolution.

## 8. Creating an incarnation

Incarnation is a runtime service instance (lives in Postgres: `spec` / `state` / `status`). Creating an incarnation runs the `create` script on the target hosts. We bind to coven `demo`, where our Soul is:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hello-demo",
    "service": "hello-world",
    "covens": ["demo"],
    "input": { "greeting": "hello from my first service" }
  }'
```

Reply `202 Accepted` with `apply_id` is an asynchronous operation. Endpoint contract - [keeper/operator-api/incarnations.md](../keeper/operator-api/incarnations.md).

`input.greeting` here is the same parameter from `scenario/create/main.yml`; Keeper validates it against the script's `input:` contract before running.

## 9. Apply and check

**Incarnation status** (`applying` → `ready` on success, `error_locked` on failure on at least one host):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo -H "Authorization: Bearer $TOKEN"
```

The same via the CLI (`soulctl` is a thin wrapper over the Operator API):

```sh
soulctl incarnation get hello-demo
```

**Result on the host** - with `status: ready`:

```sh
cat /tmp/soul-stack-hello        # → hello from my first service
```

**State and history.** In `incarnation.state.greeting_file` - the path to the created file (what `state_changes.sets` wrote). History of runs (snapshots in `state_history`):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo/history -H "Authorization: Bearer $TOKEN"
# or
soulctl incarnation history hello-demo
```

### Rerun script

Creating an incarnation runs `create` once. To run the script on an **existing** incarnation (the same `create` or another service operation), there is `soulctl incarnation run`:

```sh
soulctl incarnation run hello-demo create --input '{"greeting":"hi again"}' --wait
```

`--wait` polls the status until apply completes. Summary of ways to run a job (scenario / batch via Voyage / single-Errand / push) - [keeper/run-flavors.md](../keeper/run-flavors.md).

## 10. What's next

You've put together a single-script service. Further - as it grows:

- **More operations.** Add scripts `scenario/<op>/main.yml` (`add_user`, `restart`, …) - each with its own `input:` and `state_changes:`. Complete DSL grammar of tasks (loop / block / register / onchanges / retry / ...) - [docs/destiny/tasks.md](../destiny/tasks.md); orchestration delta scenario (`on:` / `where:` / `serial:` / `apply:`) - [docs/scenario/orchestration.md](../scenario/orchestration.md).
- **Template files.** When the content is more complex than one line - Go text/template in `scenario/<name>/templates/<path>.tmpl` + module `core.file.rendered`. Templating engine spec - [docs/templating.md](../templating.md).
- **Structural state and migrations.** When state outgrows one or two fields and changes incompatiblely, raise `state_schema_version` and add `migrations/<NNN>_to_<MMM>.yml`. Migrations format (flat DSL + CEL + `foreach`, forward-only) - [docs/migrations.md](../migrations.md).
- **Dependencies.** Reused task packages - move them to separate destinies and connect them via `destiny[]` to `service.yml` + `apply:` in the script. Custom modules - via `modules[]`. Format - [docs/service/manifest.md](../service/manifest.md).
- **Host facts.** Targeting and values ​​for system facts - `soulprint.self.*` (OS-family, pkg_mgr, IP, ...). Scheme - [docs/soul/soulprint.md](../soul/soulprint.md).
- **Day-2 operations** (monitoring, upgrade, cluster restoration) - section [To do](../README.md) in the documentation map and [docs/operations/](../operations/README.md).
- **Ready samples** of more complex services - [`examples/service/`](../../examples/service/) (for example, [`redis/`](../../examples/service/redis/) - one service for all deployment modes standalone/sentinel/cluster/sentinel_only with day-2 rolling-restart and reshard).
