# Destiny folder layout and format `destiny.yml`

Describes the physical layout of one destiny repo and root manifest fields. For the content side (what destiny is, how it relates to its neighbors), see [concept.md](concept.md); task format - in [tasks.md](tasks.md); `input:`-contract - in [input.md](input.md); `vars:` locals - in [vars.md](vars.md).

## Folder layout

```
destiny-<name>/
├── destiny.yml                     # manifest (this document)
├── vars.yml                        # opt.: destiny-locals (see vars.md)
├── tasks/
│   ├── main.yml                    # entry point; see tasks.md
│   ├── install.yml                 # opt.: include-neighbors main.yml
│   └── restart.yml                 # opt.
├── templates/                      # opt.: text/template templates for core.file.rendered (ADR-010)
│   └── *.tmpl
└── tests/                          # opt.: molecule-style tests
    └── <case-name>/
        ├── case.yml
        ├── prepare.yml             # opt.
        ├── verify.yml              # opt.
        └── cleanup.yml             # opt.
```

Only `destiny.yml` and `tasks/main.yml` are required. Everything else appears as needed.

## `destiny.yml` - manifest

The root file is **only** the manifest. There is no list of tasks in it; he lives in [`tasks/main.yml`](tasks.md). This is done deliberately: `destiny.yml` remains short - name, description, contract for inputs and dependencies on custom modules; can be read in one glance during review.

### Fields

| Field | Obligation | Meaning |
|---|---|---|
| `name:` | yes | Short kebab-case name destiny (`redis`, `haproxy`, `cert-rotation`). Same as folder name `destiny-<name>/` without prefix. |
| `description:` | recommended | One or two phrases in English: what destiny does on the host. Visible in UI Keeper, MCP directory, output `soul-lint`. |
| `input:` | yes (if there are parameters) | Entry contract. Format - general standard [`docs/input.md`](../input.md); destiny-specifics - in [input.md](input.md). |
| `output:` | no (yes = destiny returns the result to the caller) | Exit contract. **Symmetrical to `input:`** in shape - same general standard [`docs/input.md`](../input.md); destiny-specifics - in [output.md](output.md). Optional: if destiny doesn't publish anything to the outside, the block is omitted. ★ The block parses and validates, but **nothing fills it yet** - the machinery that would move a value into it is unbuilt (a task-level `output:` is refused, `output_unsupported`). |
| `validate:` | no | Declarative **input invariants**: a list of `[{that: <CEL-bool>, message: <str>}]` rules over the destiny's own `input:` ([ADR-009](../adr/0009-scenario-dsl.md) amendment 2026-07-26). Same grammar and same validator as the scenario section ([scenario/orchestration.md §2.5](../scenario/orchestration.md)), but a strictly input-only context: the destiny pass is isolated (ADR-009 V2), so `incarnation.*` is refused there rather than answered — a run-level value reaches a destiny only through `apply: input:`. Enforced at render — see ["Where is validated"](input.md#where-is-validated). |
| `compat:` | no | Declared **engine-compatibility window** — which keeper versions this destiny was tested against ([ADR-0076](../adr/0076-engine-compat-window.md)). One key today: `keeper: {min, max}`. No block → unbounded. See ["`compat` Section"](#compat-section). |
| `required_modules:` | no | List of **custom** modules the tasks need, each as the registration alias (`redis`) or the alias with the object it addresses (`redis.instance`) — **both depths are canonical here**, unlike `service.yml → modules[]`, which installs and therefore declares artifacts only ([NIM-829](../service/manifest.md#one-row-per-artifact-transition-window-to-2026-12-01)). This list carries no `ref` and synthesizes nothing, so naming the object is a more precise statement with no contradictory state behind it. Core modules **are not listed** - they are always available. See [architecture.md → "Addressing modules"](../architecture.md). |

### What is NOT in `destiny.yml`

- **The task list itself.** Located in `tasks/main.yml` as a top-level YAML list (see [tasks.md](tasks.md)). If you see `tasks:` or `steps:` as a key in `destiny.yml`, this is an outdated format.
- **Destiny locals (`vars:`).** They are located in `vars.yml` next to `destiny.yml` as a top-level YAML-map (see [vars.md](vars.md)).
- **`version:`.** The destiny version is git ref, under which the file is committed. See [ADR-007](../adr/0007-versioning-git-ref.md). The extension of the `output:` contract is an evolution of the contract, **not** a reason to introduce `version:`; rule ADR-007 applies to `output:` in the same way as to `input:`.
- **`templates:` / `tests:` sections.** These are **folders** on disk, not manifest fields. The content is picked up according to convention.

### Example

```yaml
# redis/destiny.yml
name: redis
description: Install and configure Redis server on a single host

input:
  action:
    type: string
    required: true
    enum: [apply, ensure_user, restart, ping, replication_status]
  version:
    type: string
    format: semver
  password:
    type: string
    secret: true
    min_length: 16
  port:
    type: integer
    # Conditional requiredness - a pure function of the rest of the input.
    required_when: "input.action == 'apply'"
  # ... other parameters - see examples/destiny/redis/destiny.yml

# Cross-field invariants no single input: key can express. A false rule rejects
# the render with its own message:, before anything reaches a host.
validate:
  - that: "input.action != 'apply' || input.version != ''"
    message: "action=apply requires an explicit version"

# This destiny uses only core modules → required_modules is not needed.
# Appears only when custom modules from third-party collections are needed:
#
# required_modules: [haproxy.instance, haproxy.backend]
```

A working example with a complete `input:` block is in [examples/destiny/redis/destiny.yml](../../examples/destiny/redis/destiny.yml).

## `compat` Section

An optional top-level section declaring the **keeper versions this destiny is known to work with** ([ADR-0076](../adr/0076-engine-compat-window.md)).

```yaml
# redis/destiny.yml
compat:
  keeper:
    min: "0.1.0"   # inclusive
    max: "0.3.0"   # EXCLUSIVE - the first keeper version NOT tested
```

The grammar is identical to the service manifest's — plain `MAJOR.MINOR.PATCH`, no `v` prefix, no pre-release suffix, no operators; both keys optional, a block with neither is `compat_window_incomplete`, and `min >= max` is `compat_window_empty`. Full semantics, the intersection rule and the enforcement points: [`docs/service/manifest.md → compat Section`](../service/manifest.md#compat-section).

**The declaration is cross-checked against the body.** `soul-lint validate-destiny` weighs the declared window against what `destiny.yml` and its `tasks/main.yml` actually use — a `min` below the version that introduced a used feature is `compat_floor_too_low` ([`docs/service/manifest.md → compat Section`](../service/manifest.md#compat-section)). A diagnostic, not a run-time block.

**Why the destiny declares its own.** A destiny is a separate git artifact pinned at its own `ref:` ([ADR-007](../adr/0007-versioning-git-ref.md)): it is authored, tested and upgraded independently of the service that consumes it, so only it can state its own tested range. The window in force for a run is the intersection of the service's and every destiny's — the narrowest wins. Keeper checks each declaration as it resolves the destiny, so the rejection names **this** destiny and its ref rather than the service as a whole.

## When you need neighbors `tasks/main.yml`

One `tasks/main.yml` copes as long as destiny remains an atomic brick. If the file goes beyond ~150 lines or logical subsections are clearly allocated inside, we move them to the neighbors of `tasks/<sub>.yml` — or one level down, into `tasks/<dir>/<sub>.yml` — and connect them through `include:`. Comparison with the scenario, where `scenario/<name>/main.yml` is immediately projected onto its include neighbors (`install.yml`, `replication.yml`, etc.):

```yaml
# tasks/main.yml — top-level list of tasks, without a wrapper.
- name: Install redis-server package
  module: core.pkg.installed
  when: input.action == 'apply'
  params: { name: redis-server, version: "${ input.version }" }

- name: Apply Redis configuration
  include: configure.yml             # connects neighboring tasks/configure.yml
  when: input.action == 'apply'

- name: Restart redis-server
  module: core.service.restarted
  when: input.action == 'restart'
  params: { name: redis-server }
```

`include:` includes a file from the folder `tasks/`, or from one subdirectory of it (`include: shared/probe.yml`) when several neighbors share a body — deeper than one level is a validation error. The exact syntax of include (calculation of `when:`, scope of variables, processing of `register:` across the border) - see [tasks.md](tasks.md). Depth of nesting - according to conviction, without a hard limit; in practice, level 1 covers all realistic scenarios.

## See also

- [tasks.md](tasks.md) - format `tasks/main.yml`.
- [input.md](input.md) - destiny-specific `input:`.
- [output.md](output.md) - destiny-specific `output:` (symmetric document).
- [testing.md](testing.md) — `tests/<case>/` layout.
- [../service/manifest.md](../service/manifest.md) - format `service.yml` (level above destiny: service type, scenario operations, state_schema, migrations).
- [ADR-007](../adr/0007-versioning-git-ref.md) - why is `version:` missing.
