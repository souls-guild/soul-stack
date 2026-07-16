# Modules and the cache on the Soul host

This section is about the **host side** of modules: where they physically live, how they reach the host, how they are cached, and how they are cleaned up. For the module model itself (core vs custom, the `<namespace>.<module>.<state>` addressing, the gRPC-stdio protocol, the manifest) — see [architecture.md → Module model](../architecture.md#модель-модулей); it is deliberately not duplicated here.

## Layout on the host

```
/var/lib/soul-stack/
  bin/
    soul-<sha>                 # current version + 1–2 previous for rollback
  modules/
    community-redis/           # catalog slot of a custom module: <ns>-<name>/
      manifest.yaml            #   materialized from PluginSigil.manifest_raw
      soul-mod-redis           #   binary (single-active, atomic rename)
    wb-haproxy/
      manifest.yaml
      soul-mod-haproxy
```

- **`bin/soul-<sha>`** — the agent executable itself. The name contains the SHA-256 of the binary, which allows keeping several versions side by side and rolling back without re-downloading. Used by the push mode (Keeper rolls out the binary over SSH); in the pull mode updating the daemon is the operator's task (a systemd unit, a package manager).
- **`modules/<ns>-<name>/{manifest.yaml, soul-mod-<name>}`** — the catalog slot of a custom module ([ADR-065](../adr/0065-core-module-installed.md); the names — [naming-rules.md → Destiny modules](../naming-rules.md#модули-destiny)). **Single-active** per `(namespace, name)` pair: one active version, writing via an atomic rename; several versions are not kept side by side — the authority = the active Sigil grant, a "rollback" = revoke+allow of another grant on Keeper + a repeated install step. `manifest.yaml` is materialized from `PluginSigil.manifest_raw` (it arrives in a `SigilSnapshot`, not via a fetch). The binary is launched by the `soul` binary as a sub-process over gRPC-stdio.
- **Core modules do not lie on disk.** They are statically built into the `soul-<sha>` binary.

The path `/var/lib/soul-stack/modules/` is configured via `paths.modules` in [`soul.yml`](config.md#paths). The path to `bin/` is currently fixed by convention (the push binary is rolled out into it).

## Behavior in pull (agent mode)

- When applying a Destiny step, the `soul` daemon invokes a built-in core module or a sub-process of a custom module.
- Delivering custom modules to the host — via the Destiny itself: the built-in core module `core.module.installed` ([ADR-065](../adr/0065-core-module-installed.md)) — an allow-check against the local Sigil set **before** the fetch (no active grant → `module_not_allowed` without a single network byte) → the server-streaming RPC `FetchModule` from Keeper → a full verify (sha256 + the Sigil signature + `manifest_sha256`, `shared/pluginhost`) → an atomic rename into the catalog slot → hot-register without restarting the daemon (the module is available to the tasks of the same run). Idempotency: the sha256 of the installed binary == the sha of the active Sigil → `changed=false`, the fetch is not performed. This is an ordinary Destiny operation, nothing magical.
- The daemon does not try to "guess" which modules will be needed ahead of time: if a needed module is absent at apply time — the step fails. The install step before the first consumer of a module is usually **synthesized by Keeper itself** from the `service.yml::modules[]` declaration ([ADR-065 amendment](../adr/0065-core-module-installed.md); the canon — [keeper/modules.md → Auto-synthesis](../keeper/modules.md#авто-синтез-coremoduleinstalled-из-serviceymlmodules)) — for Soul a synthesized step is indistinguishable from an explicit one, the delivery/verify/cache do not change. An explicit step remains for takeover (its own position/`ref`/`when:`) and for modules used only inside a destiny.

## Behavior in push (keeper.push)

- Keeper delivers to the host **all modules registered in Keeper** (without static analysis of the Destiny). Comparison by SHA-256 per module; nothing changed — the copy is skipped. This works thanks to the hot cache on the host (the same catalog slots `<ns>-<name>/` as in pull).
- The `soul` binary itself is rolled out by the same mechanism: Keeper compares the SHA-256 of the target version with what lies in `bin/`, and copies only on a mismatch.
- The first run on a new host is slow (the binary and all modules are copied). Subsequent ones are instant.
- For `bin/`, the file names with a SHA suffix allow keeping several agent versions side by side and rolling back without re-downloading; module slots are single-active ([ADR-065](../adr/0065-core-module-installed.md)).

The full push-delivery algorithm is in [keeper/push.md → Delivering the `soul` binary and modules to the host](../keeper/push.md#доставка-soul-бинаря-и-модулей-на-хост).

## Local cache cleanup

The `Reaper` in Keeper works only over Postgres — it **does not go** to hosts over SSH and does not clean local files. This is a deliberate decision: otherwise the Reaper would have to be granted SSH rights over all Souls, which is bad from a blast-radius standpoint. Host cleanup is arranged differently:

### In pull mode

The `soul` daemon periodically (on the schedule from its config) deletes in `/var/lib/soul-stack/bin/` and `/var/lib/soul-stack/modules/` the versions that were not used for N days.

The parameters — in [`soul.yml` → cleanup](config.md#cleanup):

| Parameter | Meaning |
|---|---|
| `cleanup.modules_ttl_days` | How many days an unused version lives before deletion. |
| `cleanup.run_interval` | How often the daemon runs a pass over the cache. |

### In push mode

Cleanup happens within `keeper.push` itself: when connecting to a host, Keeper may optionally (by a policy flag) compare the local cache with the module registry and delete stale versions in the same SSH session. The parameters on the Soul side are not used in this case — the push host does nothing between runs.

### On revoke / host removal

The operator may initiate `keeper.push.cleanup` — a separate push operation that wipes `/var/lib/soul-stack/` entirely on the specified host. Applied on revoking (`revoke`) a Soul or removing the host from the registry.

## The SoulModule manifest

Each custom module declares itself in a **static `manifest.yaml`** at the root of the plugin repo and next to the binary in the host cache ([ADR-020(a)](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle)). `soul-lint` parses the file **without running the binary** for static destiny validation.

The manifest format is **unified for all three plugin kinds** (`soul_module` / `cloud_driver` / `ssh_provider`) with a `kind:` discriminator. The normative source on the manifest fields, handshake, lifecycle, capabilities, side_effects is **[`../keeper/plugins.md`](../keeper/plugins.md)**. Here — only the specifics of `kind: soul_module`.

### spec: for kind: soul_module

The kind-specific `SoulModule` block is `spec.states`: a map of supported states with an input schema for each.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `spec.states` | `map<state-name, {input, description?}>` | — | A map of supported module states. The key is the state name (`installed` / `running` / `run` / …, see [naming-rules.md → Destiny modules](../naming-rules.md#модули-destiny)). |
| `spec.states.<name>.input` | input-schema ([`docs/input.md`](../input.md)) | `{}` | The parameter contract for this state. `soul-lint` validates the `params:` of each destiny task against this schema. |
| `spec.states.<name>.description` | `string` (optional) | — | A human-readable description for documentation / UI. |

The full table of common manifest fields (`kind`, `protocol_version`, `namespace`, `name`, `required_capabilities`, `side_effects`), the normative handshake JSON schema, the lifecycle diagram, and the enum tables are in **[`../keeper/plugins.md`](../keeper/plugins.md)**, deliberately not duplicated here.

### Example: soul-mod-haproxy

```yaml
# soul-mod-haproxy/manifest.yaml
kind: soul_module
protocol_version: 1
namespace: wb
name: haproxy

required_capabilities:
  - run_as_root
  - exec_subprocess

side_effects:
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }
  - { package: haproxy }

spec:
  states:
    running:
      description: HAProxy is running and enabled in systemd.
      input:
        name:        { type: string, required: true }
        enabled:     { type: boolean, default: true }
        config_path: { type: string, default: /etc/haproxy/haproxy.cfg }
    stopped:
      description: HAProxy is stopped.
      input:
        name: { type: string, required: true }
    restarted:
      description: HAProxy restarted (force-restart).
      input:
        name:        { type: string, required: true }
        config_path: { type: string, default: /etc/haproxy/haproxy.cfg }
    reloaded:
      description: HAProxy reload (SIGHUP) without downtime.
      input:
        name: { type: string, required: true }
```

The destiny step addressing is `<namespace>.<name>.<state>`, in the example above — `wb.haproxy.running` / `wb.haproxy.stopped` / `wb.haproxy.restarted` / `wb.haproxy.reloaded`.

### Core modules and the manifest

**Core modules** (statically built into the `soul` binary, see [naming-rules.md → Destiny modules](../naming-rules.md#модули-destiny)) do without a separate `manifest.yaml` file next to the binary: their declaration is embedded into the registry at compile time (`go:embed`), and the table of states and input schemas is available to `soul-lint` through the same `shared/plugin` parser as for custom modules, but without reading from disk. The declaration format is the same `kind: soul_module` format from [`../keeper/plugins.md`](../keeper/plugins.md).

Implementation:

- The manifests lie as `*.yaml` next to the registry in the **`shared/coremanifest`** package (one file per core module: `exec.yaml`, `file.yaml`, …). Placement in `shared/` was chosen for isolation: both `soul` and `soul-lint` import `shared/`, but they do not import each other and do not pull `keeper` — the compiler guarantees that the linter does not pull in the runtime module implementations.
- When validating a destiny/scenario, `soul-lint` finds for each task `module: core.<m>.<state>` the manifest in the registry, takes `spec.states.<state>.input`, and checks `params:`: an unknown parameter (`command` instead of `cmd`), a missing required (`cmd`/`path`), a literal type mismatch. A structural check by `plugin.InputParamDef` (type/required/secret/pattern); enum, numeric bounds, and nested object/array schemas are not expressible in this DSL — deferred until the unification of `config.InputSchema`↔`plugin`.
- The manifest describes the **author-facing** contract — what the operator writes in `params:`. For `core.file.rendered` this is `template:` (the path to the `.tmpl`) + `vars:`, and **not** the runtime form `template_content`+`render_context` that Keeper substitutes after the CEL/text-template phases ([ADR-010](../adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов), [ADR-012](../adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add)). Therefore the runtime `Module.Validate` of modules with a handoff transformation of params (rendered) validates its runtime form separately; for modules without a handoff (`core.exec`) the runtime `Validate` delegates to the same manifest registry — a single source of per-field checks.
- Keeper-side core (`core.soul`/`core.cloud`/`core.vault`, [ADR-017](../adr/0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)) is added to the registry by the same mechanism (a new `<module>.yaml`).

## See also

- [config.md](config.md) — where `paths.modules` and `cleanup.*` are set.
- [identity.md](identity.md) — revoking a Soul as a trigger for `keeper.push.cleanup`.
- [architecture.md → Module model](../architecture.md#модель-модулей) — core vs custom, addressing, the manifest, the gRPC-stdio protocol.
- [architecture.md → Host behavior and cleanup](../architecture.md#поведение-на-хосте-и-cleanup) — a short overview and the "DB vs host" boundary.
- [architecture.md → ADR-020](../adr/0020-plugin-infrastructure.md#adr-020-plugin-инфраструктура-формат-manifest-handshake-lifecycle) — the normative decision on the plugin infrastructure.
- [`../keeper/plugins.md`](../keeper/plugins.md) — the **normative source** on the manifest, handshake, lifecycle, capabilities, side_effects (the format is unified for all three kinds).
- [keeper/push.md](../keeper/push.md) — the push algorithm and the delivery of the `soul` binary/modules from the Keeper side.
- [naming-rules.md → Destiny modules](../naming-rules.md#модули-destiny) — the vocabulary of names (`soul-mod-<name>`, core modules, custom modules).
