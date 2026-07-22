# Destiny - concept

Destiny is an atomic declarative brick of "how to bring one host into the desired state." In the Soul Stack dictionary, destiny is the declarative unit describing the desired state of a single host, without a broader orchestration scope.

## What is destiny

- **Atomicity.** One destiny is responsible for one thing: "how to install and configure Redis on the host", "how to roll out the haproxy config", "how to rotate an SSL certificate". Not "how to deploy an entire cluster" - this is the service level (see below).
- **Declarative.** Destiny content is **the desired state of the host**, not "commands to be executed". Steps are formulated through module addresses of the form `core.pkg.installed`, `core.service.running` - read as "package installed", "service started", and not "install package", "start service". See [architecture.md → "Addressing modules"](../architecture.md).
- **Idempotency.** Reapplying destiny with the same input parameters should leave the host in the same state. This is an invariant, not a "possible" property. A double run in destiny-testing checks exactly this - see [testing.md](testing.md).
- **Without runtime-state.** Destiny does not know about the Keeper database, about other hosts, about the current state of the cluster, **and does not see the essence of the service**. Everything she needs from the outside world comes through the **`input:`-contract**; The author sets local values in **`vars.yml`** (isolated, not overridden from outside). This is how destiny differs from scenario, see below.

> **The destiny / scenario boundary is a recommendation, not an invariant** ([ADR-009](../adr/0009-scenario-dsl.md)). The previous invariant "scenario only `apply:`, without `module:`" has been removed: scenario received the entire DSL core of destiny tasks (including those changing `module:`) plus orchestration. At the same time, destiny **remains** an isolated, independently versioned, molecule-tested brick - but the choice of "move to destiny vs inline to scenario" is now determined by a recommendation (reused/critical → in destiny), and not a ban. The destiny properties from this list do not change; only the status of the border changes. Details - [`docs/scenario/concept.md`](../scenario/concept.md).

## Where is destiny in the big picture

```
operator ──► incarnation.spec   (what is declared)
                  │
                  ▼
          scenario(create/restart/…)
                  │
       ┌──────────┴──────────┐
       ▼                     ▼
     on: keeper         on: [coven, …]
       │                     │
       ▼                     ▼
     destiny             destiny   ◄── atomic brick
       │                     │
       ▼                     ▼
   module(state)        module(state)
```

| Layer | Who | What does |
|---|---|---|
| **Service** | `service.yml` + `scenario/<name>/main.yml` in the service git repo | Service type (Redis HA), set of operations (`create`, `add_user`, `restart`, …). Glues destiny to different hosts through the `tasks:` scenario, reads/writes `incarnation.state` to the database. |
| **Scenario** | `scenario/<name>/main.yml` | A specific operation on a service. May mix `on: keeper` (cloud-create, vault-resolve) and `on: [coven, …]` / omitted `on:` (Souls execution); volatile per-host filter - `where:`. See [scenario/orchestration.md](../scenario/orchestration.md). |
| **Destiny** | `destiny-<name>/destiny.yml` + `tasks/main.yml` | Atomic declaration "how to bring the host into state X". Doesn't know about the database, about cluster topology, about other Souls. Accepts values via `input:`. |
| **Module** | `core.pkg.installed`, `acme.haproxy.reloaded`, … | Implementation of one "verb" (install the package, start the service, render the file). See [architecture.md → "Module model"](../architecture.md). |

Service `tasks:` ↔ destiny `tasks:` - different entities with the same name (see footnote in [tasks.md](tasks.md)):

- Service-task - `apply: { destiny: redis, input: {...} }` to specific `on:`. It's "cast destiny on multiple Souls."
- Destiny-task - `module: core.pkg.installed` with `params:`. This is "call one state form of a module on one host."

## How is destiny different from its neighbors

| | Destiny | Scenario | Module |
|---|---|---|---|
| **Level** | one host | one cluster | one operation (verb) |
| **Knows about other hosts?** | no | yes (via `on:`/`where:` and `soulprint.where`) | no |
| **Writes state to the database?** | no | yes (`state_changes`) | no |
| **Parameterizable?** | yes (`input:`) | yes (`input:`) | yes (`params:` steps) |
| **Task DSL** | [tasks.md](tasks.md) | same core + orchestration delta ([scenario/orchestration.md](../scenario/orchestration.md)) | — |
| **Isolation / `module:`** | isolated, sees only `input:` | border - recommendation, `module:` allowed ([ADR-009](../adr/0009-scenario-dsl.md)) | — |
| **Version** | git ref destiny-repo (see [ADR-007](../adr/0007-versioning-git-ref.md)) | git ref service-repo | git ref module-repo |
| **Distribution** | separate git repo | separate git repo | artifact-binary (custom) or built into `soul` (core) |

## Where destiny lives

One destiny = one git repository, with its own history and tags. Destiny version is git ref ([ADR-007](../adr/0007-versioning-git-ref.md)). The `version:` field in `destiny.yml` is missing - this is a conscious decision so that there is no "second truth" next to the git tag.

The complete structure of the destiny folder is in [manifest.md](manifest.md). Briefly:

```
destiny-<name>/
├── destiny.yml          # manifest: name, description, input, required_modules
├── vars.yml             # opt.: destiny-locals (see vars.md)
├── tasks/               # tasks (what used to be in steps: inside destiny.yml)
│   └── main.yml         # entry point; via include: neighbors are connected
├── templates/           # .tmpl templates for core.file.rendered (ADR-010, ADR-015)
└── tests/               # molecule-style tests of this destiny (see testing.md)
```

## See also

- [manifest.md](manifest.md) - `destiny.yml` format and folder layout.
- [tasks.md](tasks.md) - `tasks/main.yml` format and naming conventions.
- [input.md](input.md) - `input:`-destiny contract (where it is validated, how it is used).
- [vars.md](vars.md) - `vars.yml` destiny locals (which is available in task templates as `vars.*`).
- [testing.md](testing.md) - molecule-style testing of destiny on an ephemeral stand.
- [architecture.md → "Module Model"](../architecture.md) - the layer on which destiny stands.
- [architecture.md → "Service - structure and manifest"](../architecture.md) - the layer above destiny.
