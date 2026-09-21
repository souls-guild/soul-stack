# Modules and the cache on the Soul host

This section is about the **host side** of modules: where they physically live, how they reach the host, how they are cached, and how they are cleaned up. For the module model itself (core vs custom, the `<alias>.<module>.<state>` addressing, the gRPC-stdio protocol, the schema document) — see [architecture.md → Module model](../architecture.md); it is deliberately not duplicated here.

## Layout on the host

```
/var/lib/soul-stack/
  bin/
    soul                       # the agent; push overwrites this one path in place
  modules/
    redis/                     # slot of a custom module, named by the REGISTRATION ALIAS
      <schema document>        #   canonical JSON, from the artifact's trailer
      redis                    #   the single executable (single-active, atomic rename)
    acme/
      <schema document>
      haproxy                  #   named after its module; the alias above need not match
```

- **`bin/soul`** — the agent executable itself. In pull mode updating the daemon is the operator's task (a systemd unit, a package manager); in push mode Keeper rolls the binary out over SSH into `bin/soul`, overwriting in place. ⚠ The `soul-<sha>` naming this line used to describe — several versions side by side for rollback without re-downloading — is a design that was never built on either side.
- **`modules/<alias>/`** — the slot of a custom module ([ADR-065](../adr/0065-core-module-installed.md#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact); the names — [naming-rules.md → Destiny modules](../naming-rules.md)). **The slot is named by the registration alias** — the name the operator chose on Keeper, not one read out of the artifact, because the artifact carries none (NIM-377). **Single-active** per alias: one active version, written by atomic rename; versions are not kept side by side — the authority is the active Sigil grant, and a "rollback" is revoke+allow of another grant on Keeper plus a repeated install step.
- **The executable's filename means nothing.** The slot holds exactly one and the host takes it; the name is only a habit of the repository that built it — since NIM-851 that habit is to call the artifact after the module it serves. The `soul` binary launches it as a sub-process over gRPC-stdio, naming the module it wants as a **subcommand** (`redis acl`) — one artifact serves several modules.
- **The schema document replaces `manifest.yaml`** in the slot: canonical JSON, generated from Go, stamped into the artifact as a trailer. It is also carried by `PluginSigil.manifest_raw` in a `SigilSnapshot`, which is what the allow-check reads **before** any fetch. Since the schema now travels inside the bytes as well, Keeper's signed copy and the artifact's own copy are the same bytes by construction.
- **Core modules do not lie on disk.** They are statically built into the `soul` binary.

The path `/var/lib/soul-stack/modules/` is configured via `paths.modules` in [`soul.yml`](config.md#paths). The path to `bin/` is currently fixed by convention (the push binary is rolled out into it).

## Behavior in pull (agent mode)

- When applying a Destiny step, the `soul` daemon invokes a built-in core module or a sub-process of a custom module.
- Delivering custom modules to the host — via the Destiny itself: the built-in core module `core.module.installed` ([ADR-065](../adr/0065-core-module-installed.md)) — an allow-check against the local Sigil set **before** the fetch (no active grant → `module_not_allowed` without a single network byte) → the server-streaming RPC `FetchModule` from Keeper → a full verify (sha256 + the Sigil signature + `manifest_sha256`, `shared/pluginhost`) → an atomic rename into the catalog slot → hot-register without restarting the daemon (the module is available to the tasks of the same run). Idempotency: the sha256 of the installed binary == the sha of the active Sigil → `changed=false`, the fetch is not performed. This is an ordinary Destiny operation, nothing magical. ⚠ **Only the fetch link gains a sibling with the [2026-09-04 amendment](../adr/0065-core-module-installed.md#amendment-2026-09-04-nim-794-the-fetch-step-goes-to-the-source-and-fetchmodule-stays-as-the-egress-free-path) (NIM-794 / NIM-795 — the keeper half is not implemented; the Soul half shipped as NIM-796, and with no grant yet carrying artifact rows the chain above is what every host runs).** A host that can reach the artifact source pulls the bytes **from the source**; `FetchModule` **remains** the link for hosts without egress and is not deprecated. Every other link is transport-independent and unchanged — and the order above (allow-check before a single network byte, verify before anything reaches disk) is precisely what makes fetching from an untrusted source safe, so it is preserved verbatim rather than adjusted.
- The daemon does not try to "guess" which modules will be needed ahead of time: if a needed module is absent at apply time — the step fails. The install step before the first consumer of a module is usually **synthesized by Keeper itself** from the `service.yml::modules[]` declaration ([ADR-065 amendment](../adr/0065-core-module-installed.md); the canon — [keeper/modules.md → Auto-synthesis](../keeper/modules.md)) — for Soul a synthesized step is indistinguishable from an explicit one, the delivery/verify/cache do not change. An explicit step remains for takeover (its own position/`ref`/`when:`) and for modules used only inside a destiny.

## Behavior in push (keeper.push)

- The `soul` binary is rolled out over SSH from `keeper.yml::push.soul_binary_path`: Keeper compares its SHA-256 with what lies at `bin/soul` and copies only on a mismatch. The first run on a new host is slow (tens of MB); subsequent ones compare a hash and skip the upload.
- **Module delivery is designed and NOT wired** (as of NIM-869). The design is "all modules registered in Keeper, without static analysis of the Destiny, SHA-256 per module". Nothing fills `push.SoulSpec.Modules`, and the mechanism that would use it lays one flat FILE per module under `modules/`, which the alias-slot walk skips — so the slot layout has to come first. Until then a Destiny that addresses a plugin module is a pull-mode Destiny; over push only the statically built-in `core.*` modules resolve.
- **`bin/` holds one name, not one per version.** Push writes `bin/soul` and overwrites it in place; there is no `soul-<sha>` fan-out on a push host and no host-side rollback set. Module slots are single-active ([ADR-065](../adr/0065-core-module-installed.md)).

The full push-delivery algorithm is in [keeper/push.md → Delivering the `soul` binary and modules to the host](../keeper/push.md).

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

## The SoulModule schema document

A custom module declares itself as a **`module.Def` value in Go**, and the schema document is **generated** from it ([ADR-020(n)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name), NIM-377). There is no `manifest.yaml` — not in the repo, not in the slot. `soul-lint` reads `dist/schema.json` and the host reads the copy stamped into the artifact, both **without running the binary**.

The document format is **unified for all plugin kinds** with a `kind:` discriminator. The normative source on its fields, the handshake, lifecycle, capabilities and side_effects is **[`../keeper/plugins.md`](../keeper/plugins.md#schema-document)**. Here — only the specifics of `kind: soul_module`.

### `modules[]` for `kind: soul_module`

One artifact serves several modules; each declares its own states.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `modules[].name` | `string` | — | Address level 2 (`acl` in `redis.acl.present`) and the **subcommand** the host uses to select it. |
| `modules[].side` | `enum{soul,keeper}` | `soul` | Which half of the platform executes the module (NIM-747). Declared once here; every task addressing it inherits the side, and `on:` in a scenario is back to meaning "which covens" only — see [orchestration.md §3](../scenario/orchestration.md#the-side-is-the-modules-not-the-tasks). Omitting it means `soul`, which is where every module written before this field runs. Declaring `keeper` means the **Keeper** executes the module, in its own process rather than on a host (NIM-758, [keeper/modules.md → keeper-side plugin modules](../keeper/modules.md#keeper-side-plugin-modules)); such a scenario still writes `on: keeper` on the task, because a scenario cannot read this document to derive the side. A module declaring `soul` that a keeper-side step addresses is refused by name, not silently sent to a host. ⚠ **The gate is one-directional: the Soul host does not read this field.** A `side: keeper` artifact distributed to a host is still executable there — the Soul's plugin registry indexes every `soul_module` regardless — so the field records where the module is MEANT to run and is enforced only by the Keeper. Nothing declares `keeper` yet, so this is latent rather than live. |
| `modules[].capabilities` | `list<enum>` | `[]` | Disclosure to the operator, **not a control** — see [plugins.md](../keeper/plugins.md#capabilities-and-side_effects-are-disclosure). |
| `modules[].side_effects` | `list<{type: value}>` | `[]` | Disclosure to the operator, **not a contract**. |
| `modules[].states` | `map<state-name, state>` | — | The supported states. The key is the state name (`installed` / `running` / `run` / …, see [naming-rules.md → Destiny modules](../naming-rules.md)). |
| `…states.<name>.input` | input-schema ([`docs/input.md`](../input.md)) | `{}` | The parameter contract for this state. `soul-lint` validates each destiny task's `params:` against it. |
| `…states.<name>.description` | `string` (optional) | — | A human-readable description for documentation / UI. |

**Address level 1 is not in the document.** It is the registration alias the operator chose on Keeper — which is why the same artifact serves `redis.acl.present` on one cluster and `redis-community.acl.present` on another, with no rebuild.

The root fields (`kind`, `protocol_version`, `compat`), the normative handshake JSON schema, the lifecycle diagram and the enum tables are in **[`../keeper/plugins.md`](../keeper/plugins.md#schema-document)**, deliberately not duplicated here.

### Example: what an author writes

The module is named for the **object** it manages — `instance` — not for the plugin: level 2 is the object and level 3 the action ([address rule](../naming-rules.md#the-discipline-binding-the-three-levels)), so `Name: "haproxy"` here would put the plugin's own subject at level 2 and leave its other objects (`backend`, `frontend`) nowhere to go.

```go
// internal/haproxy/instance.go
var Instance = module.Def{
	Name:         "instance",
	Description:  "The HAProxy service instance on this host",
	Capabilities: []module.Capability{module.RunAsRoot, module.ExecSubprocess},
	SideEffects: []module.SideEffect{
		{Service: "haproxy"},
		{File: "/etc/haproxy/haproxy.cfg"},
		{Package: "haproxy"},
	},
	Impl: &HAProxy{},

	States: map[string]module.State{
		"running": {
			Description: "HAProxy is running and enabled in systemd",
			Input: module.Input{
				"name":        {Type: module.String, Required: true},
				"enabled":     {Type: module.Bool, Default: true},
				"config_path": {Type: module.String, Default: "/etc/haproxy/haproxy.cfg"},
			},
		},
		"stopped":  {Description: "HAProxy is stopped", Input: module.Input{"name": {Type: module.String, Required: true}}},
		"reloaded": {Description: "HAProxy reloaded (SIGHUP), without downtime", Input: module.Input{"name": {Type: module.String, Required: true}}},
	},
}
```

```go
// cmd/haproxy/main.go
func main() {
	module.ServeBundle(module.Bundle{
		Compat:  module.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []module.Def{haproxy.Instance},
	})
}
```

The build stamps the generated schema into the artifact (`soul-mod stamp`) and CI checks that the stamp still matches the code (`soul-mod verify`) — see [plugins.md → Stamping and verification](../keeper/plugins.md#stamping-and-verification) for the generated document this produces.

Destiny step addressing is `<alias>.<object>.<action>`. Registered as `haproxy`, the artifact above answers `haproxy.instance.running` / `haproxy.instance.stopped` / `haproxy.instance.reloaded`. **The `haproxy` at level 1 is not in the artifact** — an operator who registered the same bytes as `haproxy-community` would write `haproxy-community.instance.running` instead, with no rebuild. Only levels 2 and 3 are the author's.

### Core modules and their declaration

**Core modules** (statically built into the `soul` binary, see [naming-rules.md → Destiny modules](../naming-rules.md)) have no file beside a binary at all: their declaration is compiled in, and the table of states and input schemas reaches `soul-lint` through the same model as for custom modules, without reading from disk. Core modules are declared the same way plugin modules are — as Go values, the shape described in [`../keeper/plugins.md`](../keeper/plugins.md#schema-document) — so the two never diverge into separate dialects. What does not apply to them is the stamping path: they are linked into `soul`, not shipped as artifacts, so there is no trailer to read and nothing to allow-list.

Implementation:

- The declarations live in the **`shared/coremanifest`** package, one Go file per core module (`mod_exec.go`, `mod_file.go`, …). Placement in `shared/` was chosen for isolation: both `soul` and `soul-lint` import `shared/`, but they do not import each other and do not pull `keeper` — the compiler guarantees that the linter does not pull in the runtime module implementations.
- When validating a destiny/scenario, `soul-lint` finds for each task `module: core.<m>.<state>` the manifest in the registry, takes that state's `input`, and checks `params:`: an unknown parameter (`command` instead of `cmd`), a missing required (`cmd`/`path`), a literal type mismatch. A structural check by `plugin.InputParamDef` (type/required/secret/pattern); enum, numeric bounds, and nested object/array schemas are not expressible in this DSL — deferred until the unification of `config.InputSchema`↔`plugin`.
- The manifest describes the **author-facing** contract — what the operator writes in `params:`. For `core.file.rendered` this is `template:` (the path to the `.tmpl`) + `vars:`, and **not** the runtime form `template_content`+`render_context` that Keeper substitutes after the CEL/text-template phases ([ADR-010](../adr/0010-templating.md), [ADR-012](../adr/0012-keeper-soul-grpc.md)). Therefore the runtime `Module.Validate` of modules with a handoff transformation of params (rendered) validates its runtime form separately; for modules without a handoff (`core.exec`) the runtime `Validate` delegates to the same manifest registry — a single source of per-field checks.
- Keeper-side core (`core.soul`/`core.cloud`/`core.vault`, [ADR-017](../adr/0017-keeper-side-core.md)) is added to the registry by the same mechanism (a new `<module>.yaml`).

### Param strictness before Apply

The same embedded registry is also the **runtime** gate, not just the linter's ([ADR-0076](../adr/0076-engine-compat-window.md), amendment (o–q)). Before calling a module, the Soul checks the task's params against the contract **compiled into that binary**: a key the manifest does not declare fails the task with `module.unknown_param` and the module never runs.

The failure mode it closes: a Soul reads params by key, so a key it does not know is simply never read — an older agent receiving a task with a newer param used to do its old job and report OK/CHANGED while what the author asked for never happened. The [capability gate](../adr/0076-engine-compat-window.md) covers "this agent does not have the module"; this covers "it has the module but not that param".

- **The contract is this binary's, never Keeper's.** The manifest ships in the same artifact as the module (`go:embed`), so declaration and implementation cannot drift, and on a heterogeneous estate what matters is what *this* host carries.
- **The check runs on `dry_run` too.** A `Plan` that skipped it would answer "no drift" for a param it never reads — the false-clean [ADR-031](../adr/0031-scry-drift.md) forbids.
- **Transport keys are exempt on the state that owns them.** `core.file.rendered` receives `template_content`/`render_context` instead of the author's `template:`/`vars:`; those two keys are accepted on that state and nowhere else.
- **A module with no embedded manifest is unchecked** (`core.augur`) — absence of a declaration is not a declaration of absence.
- **Custom modules are advisory, not enforced.** Their schema ships in the artifact, was never enforced before, and under-declares in practice; an undeclared param is logged rather than rejected. See [ADR-0076](../adr/0076-engine-compat-window.md) (q) for the measured reason and what flipping it requires.
- **Only the unknown direction is checked here.** A missing required param stays with Keeper's static check, which sees the author's text with a line and column before a run exists.

Adding a param to a core module is therefore a Soul-side compat event: an agent that predates the param now fails loudly instead of mis-applying silently, so upgrade **souls first, then keeper** — the same order the capability gate already imposes. Shrinking a contract goes the other way, through [param deprecation](../keeper/plugins.md#deprecating-a-param).

## See also

- [config.md](config.md) — where `paths.modules` and `cleanup.*` are set.
- [identity.md](identity.md) — revoking a Soul as a trigger for `keeper.push.cleanup`.
- [architecture.md → Module model](../architecture.md) — core vs custom, addressing, the manifest, the gRPC-stdio protocol.
- [architecture.md → Host behavior and cleanup](../architecture.md) — a short overview and the "DB vs host" boundary.
- [architecture.md → ADR-020](../adr/0020-plugin-infrastructure.md) — the normative decision on the plugin infrastructure.
- [`../keeper/plugins.md`](../keeper/plugins.md) — the **normative source** on the manifest, handshake, lifecycle, capabilities, side_effects (the format is unified for all three kinds).
- [keeper/push.md](../keeper/push.md) — the push algorithm and the delivery of the `soul` binary/modules from the Keeper side.
- [naming-rules.md → Destiny modules](../naming-rules.md) — the vocabulary of names (artifact naming, core modules, custom modules).
