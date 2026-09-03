# Plugin infrastructure Soul Stack

Normative specification of the **schema document**, handshake strings, plugin lifecycle, versioning, capabilities and side_effects. The source of truth for the decisions is [ADR-020](../adr/0020-plugin-infrastructure.md). This document contains field tables, the JSON handshake schema, a lifecycle diagram, the enum tables and complete examples for all kinds of plugins.

> **Rewritten 2026-08-06 (NIM-377).** Three things a returning reader must not carry over from the previous version of this page:
>
> - **There is no `manifest.yaml`.** It is not authored, not shipped, not parsed. A module declares itself as a `module.Def` value in Go; the schema document is **generated** from it and stamped into the artifact ([ADR-020(n)/(o)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)).
> - **The artifact carries no name.** No `namespace:`, no `name:`, no binary-name convention. Address level 1 is the **registration alias** the operator chose ([ADR-020(p)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)).
> - **`required_capabilities` and `side_effects` are disclosure, not controls** — see [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure). The previous version of this page described enforcement that does not exist in the code.

The document covers **all kinds of plugins** (the schema-document format is the same, [ADR-020(e)](../adr/0020-plugin-infrastructure.md)):

> ⚠ **`cloud_driver` is being removed from this table — epic NIM-757, decided 2026-09-01, NOT implemented.**
> Every CloudDriver already *is* a plugin, so the separate contract was a duplicated abstraction: a cloud driver
> becomes an ordinary **`soul_module`** artifact declaring **`side: keeper`**, and the kind leaves the closed enum
> in `sdk/schema/` (the proto contribution is `reserved 2`, which is the never-reuse rule, not backward
> compatibility). The infrastructure this page specifies — handshake, socket, one-shot lifecycle, stamped schema
> document, Sigil gate — is untouched, which is exactly why the contract was redundant. Order: **NIM-758**
> (keeper learns to execute a keeper-side plugin; `side: keeper` on a plugin is accepted-and-inert until then)
> → **NIM-760** → **NIM-761** (removal). See
> [ADR-020 amendment 2026-09-01](../adr/0020-plugin-infrastructure.md#amendment-2026-09-01-nim-757-cloud_driver-is-removed-and-side-keeper-is-what-replaces-it).
> **The kind, its `profile_schema` root field and the `CloudDriver` contract below all ship today.**

| Kind | Host | Destination |
|---|---|---|
| `soul_module` | `soul` (agent or push) | Implements Destiny steps: [`SoulModule`](#service-contract-soulmodule). Also see [`../soul/modules.md`](../soul/modules.md). |
| `cloud_driver` ⚠ | `keeper` (module `keeper.cloud`) | Creating/deleting a VM in the cloud: [`CloudDriver`](#service-contract-clouddriver). **Slated for removal — see the note above.** |
| `ssh_provider` | `keeper` (module `keeper.push`) | SSH credentials for push run: [`SshProvider`](#service-contract-sshprovider). |
| `soul_beacon` | `soul` | Read-only host observation for Vigil: [ADR-030 V5-2](../adr/0030-vigil-oracle.md). |

**There is no binary-name column, and that is the point.** `soul-mod-<name>` / `soul-cloud-<provider>` / `soul-ssh-<provider>` used to be listed here as the naming convention; the loader computed a filename from the artifact's own `name:` and looked for it. With no self-name in the artifact there is nothing to compute — `dist/` holds **exactly one executable** and the host takes it, whatever it is called. Repositories may keep naming their output `soul-mod-redis` for the humans reading `dist/`; nothing reads it.

## Type conventions

A single type dictionary is used, as in [`config.md`](config.md):

| Record | Meaning |
|---|---|
| `string` | UTF-8 string. |
| `int` | signed 64-bit integer. |
| `bool` | `true` / `false`. |
| `path` | absolute path in the local FS of the host. |
| `enum{a,b,c}` | a string from an explicitly listed set (lowercase ASCII, no spaces). |
| `base64-pem` | base64-encoded PEM block; empty string `""` = field not used. |
| `list<T>` / `map<K,V>` | as usual. |
| `JSON Schema` | JSON Schema draft-2020-12, embedded YAML object. |

`default: —` is a required field. Optional fields are marked `optional`. Closed enum means: value expansion - via PR in `proto/plugin/vN/`, not via freeform.

## Schema document

The plugin's self-description. **Generated** from Go, never authored ([ADR-020(n)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)), and published in two places by one generator:

- **stamped into the artifact** — so Keeper always has it, whatever route the binary took;
- **written to `dist/schema.json`** — so `soul-lint` can validate a destiny without downloading the binary. An author binds it to the address their tasks use: `soul-lint validate-scenario <path> --modules redis=./soul-mod-redis/dist/schema.json` ([soul-lint.md](../soul-lint.md#plugin-module-params---modules-aliaspath)). The **alias goes on the flag** because the artifact carries none — nothing on disk could say what a task should call it.

Keeper reads it **without executing the artifact**. That constraint is not stylistic: the schema is read at `plugin.allow`, and at that moment the operator has not yet approved the binary. A design where the host runs `<plugin> --schema` to find out what it is would execute the thing it is deciding whether to trust.

**Format — canonical JSON**: sorted keys, no insignificant whitespace, one generator, byte-deterministic. Authors never read or write it, so line/column diagnostics stop earning their cost; determinism starts mattering, because these bytes are hashed and signed ([Integrity-model](#integrity-model)).

### Authoring shape

The source of truth is a `module.Def` value beside the module's own code:

```go
// internal/acl/acl.go
var Module = module.Def{
	Name:         "acl",
	Description:  "Redis ACL users",
	Capabilities: []module.Capability{module.NetworkOutbound},
	SideEffects:  []module.SideEffect{{User: "redis_acl_user"}},
	Impl:         &ACL{},

	States: map[string]module.State{
		"present": {
			Description: "The ACL user exists, enabled as requested, with the given password and rules",
			Input: module.Input{
				"host": {Type: module.String, Required: true,
					Description: "Redis host to connect to"},
				"port": {Type: module.Int, Default: 6379},
				"login_password": {Type: module.String, Secret: true, Pattern: `^vault:.*`,
					Description: "Password for login_username; MUST be a vault-ref"},
				"tls_enable": {Type: module.Bool, Default: false},
			},
		},
		"absent": {Description: "…", Input: module.Input{ /* … */ }},
	},
}

type ACL struct{ module.BaseModule }

func (a *ACL) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.GetState() {
	case "present":
		return a.present(req, stream)
	case "absent":
		return a.absent(req, stream)
	}
	return util.SendFailed(stream, "unknown state: "+req.GetState())
}
```

`Def.Name` is the **module** name — level 2 of the address (`acl` in `redis.acl.present`). It is not the plugin's name and not a namespace; the artifact has neither.

One artifact serves several modules, declared as a bundle:

```go
// cmd/soul-mod-redis/main.go
func main() {
	module.ServeBundle(module.Bundle{
		Compat:  module.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []module.Def{acl.Module, config.Module, info.Module},
	})
}
```

**Declaration and implementation cannot drift**, because there is only one of each. The self-test and the post-MVP `soul-mod gen-manifest --check` that [ADR-020(a)](../adr/0020-plugin-infrastructure.md) proposed as mitigations are unnecessary: the drift class they guarded is gone at the root.

### Stamping and verification

```make
build:
	go build -trimpath -ldflags="-s -w" -o dist/soul-mod-redis ./cmd/soul-mod-redis
	soul-mod stamp dist/soul-mod-redis        # schema into the artifact + dist/schema.json

check:
	soul-mod verify dist/soul-mod-redis       # stamped schema == code schema
```

- **`soul-mod stamp`** appends the schema to the built artifact and writes `dist/schema.json` beside it.
- **`soul-mod verify`** is the CI gate: an artifact whose stamped schema disagrees with the schema its code produces **fails the build**. This is the guard that keeps stamping honest — without it, a stale stamp would ship a description of a module that no longer exists.

**Stamping mechanism — a trailer:** the payload plus a fixed-size footer carrying its length and a magic marker, appended after the executable image. No ELF/Mach-O/PE parsing is involved — a reader takes the footer in one read from the end of the file and seeks back by the length — and ELF, Mach-O and PE loaders all ignore bytes past the image, so the artifact still runs. The magic is versioned, so a future format change makes older readers stop recognizing new artifacts rather than misread them. **Readers fail closed:** a missing or malformed trailer is an error, never an empty schema and never a fallback to a neighbouring file.

`soul-lint validate-manifest <path> [--json]` validates a schema document offline (`dist/schema.json`, or one extracted from an artifact): `kind`, the `capabilities` / `side_effects` enums, module-name and state-name grammar, `protocol_version ∈ SupportedProtocolVersions`, the kind-specific root fields, and the input DSL (type/required/secret/pattern). Exit code `0` = ok, `1` = errors, `2` = I/O fatal. The parser and validator live in `shared/plugin` — one source of truth shared with runtime discovery.

> The subcommand keeps the name `validate-manifest` — **decided, not pending** (NIM-377 wave 2): it reads a schema document, telling a `schema.json` from a stamped artifact **by content** rather than by extension. The name is a mild misnomer and was left alone deliberately; renaming a shipped subcommand costs every author's muscle memory and every script, to buy a word.

### Registration alias

The artifact has no name of its own, so **address level 1 comes entirely from registration**. The operator picks it in the catalog entry ([Plugin directory](#plugin-directory-in-keeperyml)), and it is the name every consumer sees:

| The operator registers the artifact as… | …and the destiny step is |
|---|---|
| `redis` | `redis.acl.present` |
| `redis-community` | `redis-community.acl.present` |

**Same artifact, same bytes, same digest — no rebuild.** This is what removes the collision that `namespace:` never actually prevented: two publishers both shipping something that calls itself `community.redis` used to fight over one identity, and which one an operator got depended on resolution order. Now the operator names both, because the operator is the one who knows which is which.

The alias also names the **host slot** (`<cache_root>/<alias>/…`, `<paths.modules>/<alias>/`) and, in destiny, the level-1 component of `required_modules:`. It is **not** the Sigil registry key — that keys on the artifact source ([ADR-026(a)](../adr/0026-sigil.md#amendment-2026-08-06-nim-377-the-registry-keys-on-the-artifact-source-the-signature-is-not-a-control-on-declarations) as amended); registering one artifact under two aliases is one trust decision, not two.

Aliases from the [reserved list](../naming-rules.md#reserved-namespace-names) are refused at registration. The full addressing model is **NIM-376**, a separate open ticket.

### Root fields (for all kinds)

| Field | Type | Default | Meaning |
|---|---|---|---|
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Plugin type discriminator. Closed enum; extension — via PR in `proto/plugin/vN/manifest.proto`, without breaking ([ADR-020(e)](../adr/0020-plugin-infrastructure.md)). `soul_beacon` — Soul-side event-driven monitoring plugin (ADR-030 V5-2). **`kind` is a type, not a name:** it survived the removal of `namespace:`/`name:` because each host discovers plugins by filtering on it — the soul-host keeps `soul_module` / `soul_beacon` ([`soul/internal/pluginhost`](../../soul/internal/pluginhost/pluginhost.go)) and the keeper-host keeps `cloud_driver` / `ssh_provider` / `soul_module` ([`keeper/internal/pluginhost`](../../keeper/internal/pluginhost/pluginhost.go)) — and nothing about it identifies the subject. A `soul_module` artifact passes **both** filters, which is the other half of why `side` sits per module rather than at the root. |
| `protocol_version` | `int32` | — | Version `proto/plugin/vN/`. Duplicated in the handshake line; cross-check inside the plugin and vs `SupportedProtocolVersions` host ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)). **Not artifact version** is an API compat flag, an exception to [ADR-007](../adr/0007-versioning-git-ref.md). `int32` (and not `int`) is a deliberate exception from the type dictionary: the protocol version will not grow beyond 2³¹, at the wire level the type is fixed in `proto/plugin/v1/manifest.proto`. |
| `compat` | `{keeper: <range>}` | `{}` | The engine window the artifact declares ([ADR-0076(c)](../adr/0076-engine-compat-window.md)), e.g. `{"keeper": ">=0.9 <2.0"}`. Empty = no declared bound, which an operator reads as "the author made no promise". Declared once per artifact — a bundle's modules ship together and share a version line. |
| `modules` | `list<module>` | — | **`kind: soul_module` only.** The modules this artifact serves; see [Per-module fields](#per-module-fields). Names must be unique. |
| `provider_kind` | `string` | — | **`kind: ssh_provider`.** `vault_ssh_ca` / `static_key` / `teleport` by convention, or the author's own. Affects UI/docs, **not** the `Sign`/`Authorize` contract. |
| `profile_schema` | `JSON Schema` | — | **`kind: cloud_driver`.** Schema of the VM-profile parameters, used when creating a Profile via OpenAPI/MCP (see [`cloud.md`](cloud.md)). |
| `params_schema` | `JSON Schema` | `{}` | **`kind: ssh_provider` and `kind: soul_beacon`.** Schema of the endpoint parameters — the provider params passed via env for `ssh_provider`, the Vigil `params` for `soul_beacon`. |
| ~~`namespace`~~ | — | — | **REMOVED (NIM-377).** The artifact carries no publisher and no collection. Level 1 is the registration alias (above). |
| ~~`name`~~ | — | — | **REMOVED (NIM-377).** The artifact carries no subject name. Level 2 is the **module** name, declared per entry of `modules[]`. |
| ~~`spec`~~ | — | — | **REMOVED (NIM-377).** The wrapper is gone: `modules[]` replaces `spec.states` and the three kind-specific schemas sit at the root. There is only one document shape left to wrap. |
| `binary_sha256` | `string` (hex64) | `""` (optional) | SHA-256 fingerprint of the plugin binary (hex lowercase, exactly 64 characters). Optional — empty until the **Sigil** signature ([ADR-026](../adr/0026-sigil.md)); used to verify-against-Sigil before `exec` (see [Integrity-model](#integrity-model)). Type `string` (hex), not `bytes` — consistent with `plugin_sigils.sha256` (TEXT CHECK hex64). |

### Per-module fields

`kind: soul_module` only. One entry per module the artifact serves — `acl`, `config`, `info`.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `modules[].name` | `string` (kebab-case) | — | **Address level 2** — the `acl` in `redis.acl.present`. Unique within the artifact. Also the **subcommand** the host passes to select this module (see [Lifecycle](#lifecycle)); `schema` is reserved and rejected as a module name, because an artifact must always be able to print its own document. |
| `modules[].description` | `string` (optional) | — | Human-readable description for documentation / UI. |
| `modules[].side` | `enum{soul,keeper}` | `soul` | Which half of the platform executes this module ([ADR-0087](../adr/0087-task-side-derived-from-module-address.md), NIM-747/749). **A property of the module, not of the task**: `core.state` can only be applied against the incarnation row and `core.pkg` only on a host, so the module declares the side once and every task addressing it inherits it — which is what let `on:` in a scenario go back to its single meaning, "which covens" ([orchestration.md §3](../scenario/orchestration.md#the-side-is-the-modules-not-the-tasks)). Omitted means `soul`: a module that declares nothing runs where modules have always run, so no artifact written before this key changes behaviour. **The absent key and `side: soul` are indistinguishable by design, permanently** — the zero value decodes as `soul` ([`sdk/schema/schema.go`](../../sdk/schema/schema.go)), and requiring the key, or stamping the default into stored documents later, would change every artifact's sha256 and invalidate every approval at once. An unrecognised value is `module_side_invalid` rather than the default — `side: Keeper` quietly meaning "runs on every host" is the failure the check exists for. **A keeper-side plugin is not executable yet** (NIM-688), so `keeper` here is a recorded intent the engine does not route by: such a step still fails `unknown keeper-side module`, **loudly, never as a silent skip**, and such a scenario still writes `on: keeper` on the task, which stays legal on a plugin address for exactly that reason. Not to be confused with its neighbour — **`side` is where the module runs; `side_effects` is what it touches.** |
| `modules[].introduced_in` | `string` (`MAJOR.MINOR.PATCH`) | `""` | Optional: the **engine release** in which this module first appeared ([ADR-0076(i)](../adr/0076-engine-compat-window.md)). Same grammar as a `compat:` bound — no `v` prefix, no pre-release suffix — and only a **released** version. Empty = at or before the baseline. Read from **core** manifests, whose metadata ships with the parser; on a plugin it parses but states nothing about a keeper release (a plugin has its own version line, its git ref). Published by `GET /v1/modules`. |
| `modules[].capabilities` | `list<enum>` | `[]` | Closed enum, see [capabilities table](#required_capabilities-table). **Disclosure to the operator, not a control** — see [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure). |
| `modules[].side_effects` | `list<{<resource-type>: <value>}>` | `[]` | Declared touched resources, see [side_effects table](#side_effects-table). **Disclosure to the operator, not a contract** — nothing enforces it. |
| `modules[].states` | `map<state-name, state>` | — | The supported states (or verb forms). The key is the state name (`installed` / `running` / `run` / …). |

**`capabilities` and `side_effects` sit per module, not per artifact.** They were root-level when one artifact meant one module; a bundle makes the distinction matter. Invoking `acl` must not disclose what `config` touches, and an operator approving a bundle should be able to read each module's footprint separately rather than a union that describes none of them.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `…states.<name>.description` | `string` (optional) | — | Human-readable description for documentation / UI. |
| `…states.<name>.introduced_in` | `string` (optional) | — | The engine release that added **this state**, same grammar as above. |
| `…states.<name>.input` | input-schema (see [`docs/input.md`](../input.md)) | `{}` | Contract parameters for this state — **enforced at run time**, see [The input declaration is a contract](#the-input-declaration-is-a-contract). |
| `…states.<name>.output` | same shape as `input` | `{}` | The fields the state publishes to a caller's `register:`, described with the same scheme as `input` — one vocabulary for both ends of the contract, as in destiny ([output.md](../destiny/output.md)). **Declarative disclosure only:** the engine does not yet forward module outputs into `register.<name>.<field>`, so this block documents the contract without gating anything. |
| `…input.<param>.introduced_in` | `string` (optional) | — | The engine release that added **this parameter**. The granularity that matters most: a new parameter on a long-standing state is invisible to an author, and an older engine rejects it as `unknown_param`. |
| `…input.<param>.deprecated` | `{since, removed_in, use?}` (optional) | — | The param is still honored but is on its way out ([ADR-0076](../adr/0076-engine-compat-window.md), amendment (r)). See [Deprecating a param](#deprecating-a-param). |

Parameter fields are type / required / secret / pattern / description / default, plus the [ADR-045](../adr/0045-param-dsl.md) form fields (`enum`, `format`, `source`, `multiline`, `example`) and `items` for `list`/`map` element and value types. **Nothing the old manifest could express became inexpressible** — that was a constraint on the migration, not a happy accident.

#### The input declaration is a contract

Before applying a task the Soul checks its params against the manifest that ships **beside the module binary on that host**. A key the state does not declare fails the task with `module.unknown_param` and the module never runs, so the host is untouched when it fires ([ADR-0076(o)](../adr/0076-engine-compat-window.md), amendment (t)). The same rule holds on the `dry_run` path — a plan that skipped the check would answer "no drift" for a param the module never reads.

This means an under-declared manifest is a **bug in the module**, not a cosmetic omission: a param the plugin reads but does not declare stops arriving. Declare every key the module accepts on every state that accepts it — including keys shared across states, which a header comment does not declare.

Two consequences for a plugin author:

- **The manifest on the host is the one that gates**, not the copy in a service repo. Fixing a manifest means shipping a new plugin version.
- **Removing a param is a breaking change** and goes through the deprecation window below. Adding one is safe in the only-add direction: an older definition passes a subset of what a newer manifest declares.

#### Deprecating a param

A param is **never removed outright** — that would break definitions that were valid yesterday. It is first marked deprecated, keeps working for the whole declared window, and only then leaves the manifest:

```yaml
input:
  addr:    { type: string }
  address:
    type: string
    deprecated: { since: "0.4.0", removed_in: "0.6.0", use: "addr" }
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `since` | `string` (`MAJOR.MINOR.PATCH`) | — | Release that marked the param deprecated (inclusive). |
| `removed_in` | `string` (`MAJOR.MINOR.PATCH`) | — | First release that no longer honors it — **EXCLUSIVE**, exactly like `compat.keeper.max`. |
| `use` | `string` (optional) | — | Replacement param **in the same state**; omit when there is no successor. |

Rules, all checked by `validate-manifest`:

- **Both bounds are required** (`deprecated_bound_missing`). An open-ended deprecation is a warning that never resolves — an author cannot plan a migration against it.
- **The window is at least 2 minor releases** (`deprecation_window_too_short`): deprecated in `X.Y.0` → removable no earlier than `X.(Y+2).0`. That is the same interval as the half-open window `{min: X.Y.0, max: X.(Y+2).0}` in `compat:`, and it guarantees at least one release where the old param and its replacement both work. A **major** bump is exempt — it is the declared breaking-change boundary.
- **Same grammar as `compat:`** (`deprecated_version_invalid`): plain `MAJOR.MINOR.PATCH`, no `v` prefix, no operators.
- **`use:` must name a param of the same state** (`deprecated_replacement_unknown`).

What an author sees while the param is live: a **`deprecated_param` warning** — never an error, since the param still works — naming the deadline and the replacement. At `removed_in` the key leaves the manifest and the identical task text becomes `unknown_param`.

The block is also **published by `GET /v1/modules`** beside `introduced_in`, as an object (`{since, removed_in, use}`) rather than a rendered sentence — `use` is what the UI offers as the replacement, `removed_in` is what a migration is planned by, and a prose string would have to be parsed apart again to do either. That is the surface an author reads *before* writing the task; the lint warning only reaches them once the definition exists.

`introduced_in` and `deprecated` are the two ends of the same axis, and `unknown_param` is what both converge on: before `introduced_in` and from `removed_in` onward the very same task text is rejected, in between it works.

### Why `deprecated` exists only on a parameter

`introduced_in` is declarable at three levels (module, state, parameter); `deprecated` at one. That asymmetry is deliberate ([ADR-0076(x)](../adr/0076-engine-compat-window.md)) — the levels differ in what removing them does:

| Level | What removal does today | How it is announced |
|---|---|---|
| **Parameter** | Was **silent** — a Soul reads params by key, so a key it does not know is never read and the module reports success while nothing happened | `deprecated:` — the only mechanism that can warn |
| **State / module (`core.`)** | **Loud, and early**: `module_state_unknown` from the static check with a line and column, `module.not_found` before dispatch | Release notes + those diagnostics; a manifest note would say less, later |
| **State / module (plugin)** | Governed by the plugin's own version line — its **git ref** ([ADR-007](../adr/0007-versioning-git-ref.md)) | The `ref:` the author pinned; the module does not move under them |

The engine-version axis genuinely does not reach a plugin's module: a plugin's `introduced_in` is read by nothing (the floor walk returns early on any namespace but `core`), which is why the table above says the key "states nothing about a keeper release". A module-level `deprecated` would inherit exactly that emptiness — while costing what any new manifest key costs: on an older Soul it is a decode error, and the whole slot is skipped, so the **module disappears** rather than merely losing a label.

**Deprecating a module is therefore not unsupported — it goes through a different door.** If you are retiring a plugin module, cut a new ref and say so in its release notes; consumers move when they re-pin. If a `core.` module or state is going away, that is a release-notes event, and the author meets it as a positioned lint error rather than a silent no-op.

### Which root field belongs to which kind

`modules[]` describes a **set of named modules**; the other three kinds describe a **single endpoint** and use a root-level schema instead. The validator rejects a field on the wrong kind rather than ignoring it.

> **`side` is inside `modules[]`, not beside it.** [ADR-0087](../adr/0087-task-side-derived-from-module-address.md) puts the field on the per-module object rather than at the document root precisely because of this table: a root-level `side` would land on `cloud_driver` / `ssh_provider` / `soul_beacon`, which carry no modules at all and for which "where the module runs" has no meaning — and it would foreclose one artifact serving both a keeper-side and a Soul-side module. That is where the field sits in the code: `Side` is a field of `schema.Module`, and `schema.Document` has none ([`sdk/schema/schema.go`](../../sdk/schema/schema.go)).

| Kind | Carries | Must not carry |
|---|---|---|
| `soul_module` | `modules[]` | `provider_kind`, `profile_schema`, `params_schema` |
| `cloud_driver` | `profile_schema`, opt. `provider_kind` (the provider family — `aws` / `gcp` / `yandex-cloud` / `openstack`; informational) | `modules[]`, `params_schema` |
| `ssh_provider` | `provider_kind`, opt. `params_schema` (the provider params delivered via env, e.g. `vault_mount` for `vault_ssh_ca`) | `modules[]`, `profile_schema` |
| `soul_beacon` | opt. `params_schema` (the Vigil `params` an operator sets via OpenAPI/MCP; runtime checks beyond JSON Schema go through `SoulBeacon.Validate`) | `modules[]`, `provider_kind`, `profile_schema` |

`soul_beacon` is read-only by design ([ADR-030 V5-2](../adr/0030-vigil-oracle.md) + [amendment 2026-05-26](../adr/0030-vigil-oracle.md#amendment-2026-05-26-s5-closure)): `Check` observes the host and does not mutate it, which is why it has no states.

> **Open, not decided by NIM-377.** The Go-side generator (`sdk/module`) is specified for **SoulModule bundles**. What authors the schema document for `cloud_driver` / `ssh_provider` / `soul_beacon` — whose `profile_schema` / `params_schema` are JSON Schema objects rather than a Go declaration — is not settled. The document format, the source-keyed registry, the alias-named slot and the single-executable convention apply to every kind regardless, because discovery and the slot layout are shared code.

### Schema extension

New kinds (`secrets_provider`, `audit_sink`, …) — add a variant to the `kind` enum and the corresponding root field. Forward-compat: a host of an earlier version sees an unknown `kind:` → rejects the plugin with `unknown kind=X, host supports [...]`.

Adding a field to the parameter shape is a **forward-compat event**, not a free extension: decoding is strict, so an older engine reading a newer document fails on the key it does not know ([ADR-0076(i)/(q)](../adr/0076-engine-compat-window.md)).

### Drift between declaration and code

**This class of bug no longer exists.** It used to: `manifest.yaml` was authored separately from the `Apply` it described, so an author could change one and forget the other, and [ADR-020(a)](../adr/0020-plugin-infrastructure.md) mitigated it with a self-test plus a proposed `soul-mod gen-manifest --check`. Both mitigations are now unnecessary — **there is one declaration**, in Go, and the document is generated from it.

What remains is the narrower risk that a **stamped** artifact carries an out-of-date copy of its own schema — a build that recompiled without re-stamping. That is what `soul-mod verify` exists for, and it is a CI gate rather than a runtime check because it is a build mistake, not an attack:

- **`soul-mod verify`** — stamped schema == the schema the code produces. Fails the build on a mismatch.
- **Cross-check `kind`:** `document.kind != handshake.kind` → the host refuses to start the plugin ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)).
- **Fail-closed trailer read:** a missing or malformed trailer is an error, never an empty schema.

## Handshake

When launched, the plugin writes **exactly one line** with JSON-payload to stdout. All lines up to the first with the magic field `"soul_stack":"plugin-v1"` host are **ignored** (logged at the debug level). After a handshake, stdout is considered closed to the plugin protocol - any subsequent entries to stdout are ignored by the host. Plugin logs in MVP are written to **stderr** (standard UNIX channel for diagnostics); host forwards the plugin's stderr to its log/OTel-pipeline with the tag `plugin=<namespace>.<name>`. Structured log-stream via separate gRPC-RPC - reserved under `proto/plugin/v2/`, not available in MVP.

### Handshake string format

```json
{"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/acme-haproxy-12345.sock","server_cert":""}
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `soul_stack` | `string` (constant `"plugin-v1"`) | — | Magic sanity field. Host ignores all stdout lines up to the first with this field. The value is independent of `protocol_version` - this is the "handshake-string format v1" marker; changes only when breaking changes the handshake format itself (separate ADR). |
| `protocol_version` | `int32` | — | Plugin protocol version (see [Versioning](#versioning)). Must match the schema document's `protocol_version`. The type `int32` (not `int`) is the same intentional exception as in the root fields. |
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Must match the schema document's `kind`. |
| `network` | `string` (MVP convention: `"unix"`; future `"named_pipe"` / `"tcp"`) | — | Socket type. MVP - only `unix`. Extension `named_pipe` (Windows) / `tcp` (loopback) - post-MVP, without editing `proto/plugin/vN/` (at the proto level - an open line for forward-compat). |
| `address` | `path` | — | Path to the Unix-socket on which the plugin listens to gRPC. Must match the `SOUL_PLUGIN_SOCKET` passed to env-var (see [Lifecycle](#lifecycle)). |
| `server_cert` | `base64-pem` (optional) | `""` | Reserved for optional mTLS post-MVP. In MVP there is always `""` ([ADR-020(h)](../adr/0020-plugin-infrastructure.md)). |

Expansion through new **optional** keys (`features`, `capabilities`, ...) - without breaking. The host ignores unknown optional keys.

### Host behavior during handshake

| Situation | Host behavior |
|---|---|
| stdout string is not parsed as JSON | Ignored, the next line is read. |
| stdout string is valid JSON, but without `"soul_stack":"plugin-v1"` | Ignored. |
| Handshake line appeared, but `protocol_version ∉ SupportedProtocolVersions` host | Hard fail: `protocol_version=N, host supports [...]`. SIGTERM plugin. |
| `document.protocol_version != handshake.protocol_version` | Hard fail: drift inside the plugin. SIGTERM. |
| `document.kind != handshake.kind` | Hard fail: drift inside the plugin. SIGTERM. |
| Handshake did not appear for `plugin_runtime.startup_timeout` (default `10s`) | Hard fail: startup timeout. SIGTERM, via `shutdown_grace` - SIGKILL. |
| Handshake OK, but connect to `address` failed | Hard fail: socket unreachable. SIGTERM. |
| Several lines with `"soul_stack":"plugin-v1"` | The first is handshake; all subsequent ones on stdout are ignored (after the handshake, stdout is "closed" for the plugin protocol). |

### Host behavior after handshake (plugin crash)

After a successful handshake, the plugin is a separate one-shot process that can crash before or during the Apply-stream. Host behavior:

| Situation | Host behavior |
|---|---|
| Plugin exited with exit code ≠ 0 **before Apply** (after handshake, before first RPC) | The step is labeled `failed`, reason `plugin_init_failed`. Stderr-tail (last 4KB) is reflected in the diagnostic channel `TaskEvent` / `RunResult` (the exact form of the field is a separate standardization task audit-pipeline, see backlog). |
| Plugin exited with exit code ≠ 0 **in the middle of Apply-stream** | The step is labeled `failed`, reason `plugin_crash`. gRPC-stream is closed; stderr-tail (4KB) is reflected in the diagnostic channel `TaskEvent` / `RunResult`. |
| Plugin panic / OOM-killed / SIGSEGV (any non-graceful exit) | Same behavior as above; the specific reason is best-effort from the exit code (for example, `exit_code=139` → SIGSEGV), the host writes to the diagnostic channel. |
| Retry | At the plugin-host level there is no **retry in MVP**. Retry semantics - at the scenario level through the key `retry:` (see [`../scenario/`](../scenario/README.md)). |

Reason names (`plugin_init_failed`, `plugin_crash`) are individual values in the open directory `TaskError.reason` (normalization of the full directory is a separate backlog task along with closing `proto/plugin/v1/` and audit-pipeline; see also [naming-rules.md → Host behavior after handshake](../naming-rules.md)).

## Lifecycle

Plugin - **one-shot process per Apply** ([ADR-020(d)](../adr/0020-plugin-infrastructure.md)). Long-lived (one process per series of calls) - separate ADR if necessary.

### Diagram

```
host (keeper / soul)                              plugin (the single executable in the slot)
─────────────────────                              ─────────────────────────────────────────────
   0. digest gate: verify against the Sigil BEFORE any exec (ADR-026) — a binary
      whose sha256 differs from the approved one never reaches step 3
   1. mkdir /var/run/soul-stack/plugins/  (mode 0700, owned by service user)
   2. socket_path := "/var/run/.../plugins/<alias>-<module>-<pid>.sock"
   3. fork():
      env SOUL_PLUGIN_SOCKET=<socket_path>
      exec <plugin_binary> <module>    ─────────►   ServeBundle dispatches on argv[1]
                                                    init(); read env SOUL_PLUGIN_SOCKET
                                                    listen(unix, $SOUL_PLUGIN_SOCKET, mode 0700)
                                                    register gRPC services
                                                    print one-line JSON handshake to stdout
                                       ◄─────────   (handshake bytes)
   4. read stdout, ignore lines until "soul_stack":"plugin-v1"
   5. validate handshake (protocol_version, kind, address)
   6. dial gRPC at <socket_path>
   7. RPCs (Validate / Plan / Apply / Schema / ...)
                                       ◄────►       (gRPC traffic over Unix-socket)
   8. host done. SIGTERM(plugin)        ─────────►  signal handler: finish in-flight RPCs
                                                    close gRPC server
                                                    unlink(socket_path)
                                                    exit(0)
   9. wait(grace=10s); if alive — SIGKILL.
  10. unlink per-pid socket file if still exists.
```

**The module is selected by subcommand** (`<artifact> acl`), not by which binary was forked ([ADR-020(q)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)). One artifact serves several modules, and the process serves exactly one of them for exactly one Apply. `<artifact> schema` prints the artifact's own schema document — which is why `schema` cannot be a module name.

### Lifecycle parameters (configurable via `plugin_runtime:` block)

Block `plugin_runtime:` in [`keeper.yml`](config.md) / [`soul.yml`](../soul/config.md) - regulatory specification: [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper-side) and [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul-side). Defaults are fixed in [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md); the table below duplicates them inline for ease of reading this document.

| Parameter (in `plugin_runtime:`) | Default | Meaning |
|---|---|---|
| `startup_timeout` | `10s` | Time from fork to the appearance of the handshake line. Excess → SIGTERM. |
| `shutdown_grace` | `10s` | Time from SIGTERM to SIGKILL. |
| `allowed_capabilities` | unset = **no filter** (everything allowed) | Capabilities allowed on this host. Checked by the **host at spawn**, not by `soul-lint` — see [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure). |
| `conflict_policy` | `warn` | ⚠ **Parsed and read by nothing.** Documented as the resolution policy for two plugins claiming one resource; no conflict is ever detected. See [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure). |
| `enable_tls` | `false` | Post-MVP option: enable mTLS on the plugin socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md)). |

Full field typing, value validation and per-field hot-reload policy - in [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper) and [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul).

### Socket location

| Host | Directory | Mode | Owner |
|---|---|---|---|
| `soul` | `/var/run/soul-stack/plugins/` | `0700` | service user `soul` |
| `keeper` | `/var/run/soul-stack-keeper/plugins/` | `0700` | service user `keeper` |

The socket file name is derived from the registration alias and the module (`<alias>-<module>-<pid>.sock`, e.g. `redis-acl-12345.sock`), the dot of the address replaced with a hyphen for file grammar. **The name matters for logs, not for the protocol** — the plugin reads its socket path from `SOUL_PLUGIN_SOCKET` and never constructs it. After the plugin exits the host deletes the file, in case the plugin did not unlink it itself.

## Integrity-model

The plugin binary in the host cache is forked with service-user rights (`keeper` plugins have access to Vault / PG / PKI). Substitution of the binary in `/var/lib/soul-stack-keeper/plugins/` (or in artifact-source / with Keeper-checkout git-ref **before** the first appearance on the host) → RCE. Protection - **Sigil** ([ADR-026](../adr/0026-sigil.md)): Keeper-signed digest index (**Option A**). SHA-256 reconciliation (invariant [CLAUDE.md](../../CLAUDE.md) "SHA-256 cache") is saved as defense-in-depth before each `exec`; "trust as is" with first-load **replaced** by Sigil verification.

> **Sigil replaces the previous TOFU model** (trust-on-first-use). TOFU described first-load as "the host itself considers SHA-256 and trusts the binary as is" - this did not close the substitution of the binary **until** its first appearance on the host (see ["Closed gap - first-load"](#closed-gap--first-load) below). With Sigil, the authority over which binary is allowed belongs to the Keeper, not the host.

### Root of Trust

| What | Where |
|---|---|
| **Allow-list** `(artifact source, ref) → sha256` | PG table `plugin_sigils` (Keeper-state). The entry is added **only when the Archon explicitly allows** the plugin via OpenAPI (`POST /v1/plugins/sigils`, S4a) / MCP (S4b) — permission `plugin.allow`, [rbac.md → Plugin Sigil](rbac.md#plugin-sigil-3). `ref` — **git-verified** (Keeper resolves `source`+`ref` into a `commit_sha` cache slot via go-git, [ADR-026(g)](../adr/0026-sigil.md)); chain of trust `source` → `ref` → `commit_sha` → `binary_sha256` → Keeper signature. ⚠ **The key was `(namespace, name, ref)`** and moved onto the source in NIM-377, because the artifact no longer carries a name to key on ([ADR-026(a)](../adr/0026-sigil.md#amendment-2026-08-06-nim-377-the-registry-keys-on-the-artifact-source-the-signature-is-not-a-control-on-declarations)); the migration and route shapes are **NIM-438**. The **registration alias is a different axis** — it names the address and the slot, not the trust record. Two partial unique indexes do two different jobs: `(source, ref)` is the **trust** key (re-approving one artifact is a conflict an operator resolves by revoking first, never a silent second grant); `(alias)` is a **registration** invariant, so `<alias>.<module>.<state>` names one set of bytes and the runtime lookup has exactly one answer. |
| **Keeper signing key** (private) | Vault KV - according to the pattern `secret/keeper/jwt-signing-key` ([ADR-014](../adr/0014-operator-identity.md)). |
| **Keeper public key** (trust-anchor host) | Soul arrives in **bootstrap** along with a CA-chain (the same channel `BootstrapReply` as mTLS CA, [ADR-012(f)](../adr/0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add)): single `sigil_pubkey_pem` or multi-anchor `sigil_pubkey_pem_set` (priority set > single). The runtime set of anchors is delivered to `SigilTrustAnchors` and **completely replaces** bootstrap-anchors (replace, not merge; R3 rotation) - see [Active set and replace semantics](#active-set-and-replace-semantics). |

**Sigil** = `sign_keeper(block)`, where the signed block carries the artifact identity, the `ref`, `binary_sha256` and the schema-document hash. The signature **covers the schema document** with the attached `binary_sha256` ([ADR-026(c)](../adr/0026-sigil.md)): the declared `side_effects` / `capabilities` / `protocol_version` cannot be substituted without breaking the signature.

> **What that buys, precisely.** The operator's **disclosure** becomes trustworthy — what they read at `plugin.allow` is what the artifact actually claimed, so an attacker cannot pair a hostile binary with a reassuring declaration. It does **not** make those declarations enforceable; nothing enforces them ([Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure)). `protocol_version` is the one signed field with a consumer that acts on it. **The control is the digest**, and it consults no declaration.

> **`ref` - git-verified** ([ADR-026(g)](../adr/0026-sigil.md), **Option A, F-fetch**). Keeper itself resolves `source`+`ref` from the `keeper.yml` directory via **go-git**: shallow `clone`→`fetch`→`ResolveRevision(<ref>^{commit})` (resolved in 40-hex `commit_sha`)→detached-HEAD `checkout`, then extracts the **ALREADY compiled** binary — **the single executable in `dist/`** — and reads the schema document from its trailer (F-fetch: no compilation on Keeper, and no execution of the artifact either). Boundary "verified" = "Keeper checked this particular `ref` and recorded the result (`commit_sha` + `binary_sha256`)", **NOT** bit-reproducibility of the assembly. Cache - **R-nested**: `<cacheRoot>/<alias>/<commit_sha>/` (immutable slot) + symlink `current → <commit_sha>` (atomically permutable pointer to the active slot). **Single-active-per-slot**: `current` points to exactly one `commit_sha`, but multiple `commit_sha` slots under one alias coexist. `plugin.allow` reads the binary + schema of the ACTIVE slot via `current` ([`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go)), reads `sha256`, signs and inserts the record; `ref` is not involved in the slot lookup. **Integrity Authority = `sha256` + signature** (invariant (b) ADR-026 not weakened); `ref`/`commit_sha` carry provenance and audit-readability, not trust. `commit_sha` — audit mark OUTSIDE the signature; will be added as a column to `plugin_sigils` at S3.

### Signed block format (normative, S3)

The block is assembled with a pure deterministic function (`shared/pluginhost.BuildSigilBlock`) - common code for signature on Keeper (S3) and verification on Soul (S6), **without** proto-marshal (proto-serialization is non-deterministic - it was deliberately excluded):

```
block = DST || LP(source) || LP(ref) || LP(binary_sha256) || LP(schema_sha256)
```

> **Re-keyed onto the artifact source (NIM-377 / NIM-438, landed).** The block used to carry `namespace` and `name`; the artifact declares neither, so the identity it is bound to is now the git **source** the operator asserted, at a `ref`. **The DST moved to `soul-stack/sigil/v2`** — so every v1 signature stops verifying against this code by construction, not by accident. Migration 113 empties `plugin_sigils` for exactly that reason: those grants were already cryptographically dead, and keeping them would have shown live approvals in the UI that nothing could verify. Approvals are re-issued with `keeper.plugin.allow`.
>
> **The alias is deliberately NOT in the block.** An alias is operator-chosen text; binding a signature to it would mean the identity an approval covers is a value the same operator can rename — renaming would walk around an approved hash instead of requiring a fresh approval. The source is the one identity an operator *asserts about the bytes* rather than *picks for them*.

- **`DST`** — domain-separation tag, ASCII constant `soul-stack/sigil/v1` (without length-prefix, fixed known prefix). Version `/v1` is required: change of block format → `…/v2`, old signatures no longer work against the new code (an obvious compatibility gap). DST first → the signature over the Sigil cannot be reused in another protocol.
- **`LP(x)`** = 4 bytes of big-endian uint32 length `x`, then the bytes themselves `x`. Applies to **every** variable field - field boundary protection: without length-prefix, the concatenation of `("ab","c")` and `("a","bc")` would result in one block, and the signature over one set would fit into the other.
- Hashes (`binary_sha256`, `manifest_sha256`) are put in **raw bytes** (for SHA-256 - 32 bytes), **not** a hex string.
- The field order is fixed exactly — `source`, `ref`, `binary_sha256`, `schema_sha256` — and cannot change without bumping the DST to `/v3`.

**Signing key - ed25519** (asymmetry is required, unlike the HS256-symmetric JWT signing-key): the private person lives in Vault KV at `sigil.signing_key_ref` ([config.md → sigil](config.md#sigil)), signature - raw 64 bytes; the public part goes to Soul in bootstrap as a trust-anchor.

**S3↔S6-invariant (schema bytes).** The schema-document bytes Keeper hashes when signing must be the bytes Soul re-hashes when verifying. Since NIM-377 this holds **by construction**: the document lives in the artifact's trailer, so the schema and the binary are physically the same object and cannot be separated in transit. The document is already canonical JSON from one generator, so `NormalizeManifestBytes` collapses to identity for it — the byte-only canonicalization (strip BOM, CRLF→LF, exactly one trailing newline) that used to carry this invariant for hand-written YAML now has nothing to normalize.

**One column, not two (migration 113).** The registry stores the signed document **once**, as bytes:

- **`schema` (`bytea`, `NOT NULL`) — the canon.** Byte-exact the bytes the signature covers: the canonical-JSON schema document read out of the artifact's trailer by a single `ReadSlot` at `plugin.allow`. They are what rides in `PluginSigil.schema` and what Soul re-hashes during verify. `NOT NULL` because a grant whose signed bytes are absent cannot be verified by anything — such a row can only fail closed later, so it is rejected at write time.
- **The old `manifest_raw` (bytea canon) + `manifest` (jsonb projection) split is gone.** `manifest_raw` was renamed to `schema` and the jsonb projection was **dropped**. The split existed because a hand-written YAML manifest needed a byte-exact canon *and* a queryable form; the document is already canonical JSON, so keeping both would be two copies of one value that can disagree — and the one free to drift would be the copy the signature was **not** placed over.

### Mechanism

| Step | Host behavior |
|---|---|
| Discover | Counts SHA-256 binaries streamwise; puts in `Discovered.Digest` for logs / OTel attributes. Error reading binary for digest → plugin skipped with warning. |
| Obtaining a Sigil | **Push** (Keeper transfers plugins FROM Keeper via mTLS - Keeper is already a trust-anchor): Sigil travels with the binary. **Pull** (Soul daemon): Sigil comes only-add proto-message in `EventStream` ([ADR-026(e)](../adr/0026-sigil.md)). |
| Verify (before seal/exec) | (1) SHA-256 of actual binary == `binary_sha256` in Sigil; (2) Sigil's signature is valid with Keeper's public key (from bootstrap). Both checks passed → seal + exec. Any discrepancy → failure, **binary does not start**, event `plugin.verify_failed`. |
| Re-exec from cache | SHA-256 verification before each subsequent `exec` (defense-in-depth for shared cache). Discrepancy → failure, the binary does not start. |

Integrity-gate is triggered **before** `mkdir socket-dir` and `exec` - the invalid binary does not receive control.

### Active-set and replace-semantics

The active set of permissions and the set of trust anchors on the Soul side are maintained by **replace semantics** ([ADR-026(h)](../adr/0026-sigil.md)). **`SigilSnapshot`** is the only source of truth for the active set: Soul applies it as **ReplaceAll** (replaces its entire set, not upsert), the permission missing in the snapshot **forgets** - this is how revoke and retire work (Keeper after `plugin.revoke` sends a new snapshot without the revoked permission → near-instant revoke without restarting Soul). Single broadcast `PluginSigil` - **notification of a new admission, not a set mutation** (Soul does not upsert on it; authority - only snapshot). The set of trust-anchors (**`SigilTrustAnchors`**) is the same as replace: runtime-delivery **completely replaces** bootstrap-anchors (replace, not merge), multi-anchor supports continuous rotation of the signature key. In bootstrap, if `sigil_pubkey_pem_set` is non-empty, single `sigil_pubkey_pem` is ignored (precedence set > single); both empty = Sigil disabled.

### Signature key rotation (multi-anchor, R3)

Sigil signature keys are rotated **without breaking** verify on Souls ([ADR-026(h)](../adr/0026-sigil.md)). The `sigil_signing_keys` registry (migration 037) holds a set of keys: exactly one **primary** (with which Keeper signs new Sigils) and any number of other **active** (with which Soul also validates what was previously signed). **Private is NEVER in Postgres** - only the public part (`pubkey_pem`, SPKI) + link `vault_ref` to the private in Vault KV.

Operator-facing rotation (R3-S7, REST `/v1/sigil/keys*` + MCP `keeper.sigil.key.*`):

- **introduce** (`POST /v1/sigil/keys`, permission `sigil.key-introduce`): Keeper generates an ed25519 pair, writes the private to Vault KV (`secret/keeper/sigil-keys/<key_id>`, `key_id` = SHA-256(SPKI) hex), inserts the public part into the registry as active. The answer is `key_id` + `pubkey_pem` - **private is NEVER returned** and is not logged.
- **set-primary** (`POST /v1/sigil/keys/{key_id}/primary`, `sigil.key-set-primary`): the new key becomes primary, new Sigils are signed with it after cluster reload.
- **retire** (`DELETE /v1/sigil/keys/{key_id}`, `sigil.key-retire`): The key is removed from the set.
- **list** (`GET /v1/sigil/keys`, `sigil.key-list`): active-keys, primary first.

After each mutation, the mutating node publishes `sigil:anchors-changed` to the Redis channel; **each** node re-builds Signer (new primary + anchors) and re-broadcasts `SigilTrustAnchors` to its Souls - the set is updated near-instantly throughout the cluster. Continuous rotation: enter a new active key → make primary → the old one serves as active (Soul still trusts its signatures) → output retired.

**Retire-invariant (safety).** `Retire` is allowed only when: (1) a new set has been distributed throughout the cluster (`sigil:anchors-changed` → reload) and (2) bootstrap-reply gives a set from a **live source** (the new Soul after rotation receives the actual anchors, not a snapshot of the start). Additionally: you cannot retire **primary** directly (set-primary to another first) and **last active** (the set should not be empty - verify would lose all anchors).

### Permissions (least-privilege)

| Object | Mode | Owner | Requirement |
|---|---|---|---|
| Cache directory `<cacheRoot>/<alias>/<commit_sha>/` (R-nested per-commit slot + symlink `current → <commit_sha>`, [ADR-026(g)](../adr/0026-sigil.md)) | `0755` | service-user (`keeper` / `soul`) | Recording is for the owner only. Group/other - read-only, so that an extraneous process does not replace the binary or sidecar. |
| Plugin binary | `0755` | service-user | Executable, writable only by owner. |
| Sidecar `.sha256` | `0400` | service-user | Read-only after recording. |

Host **must** run under a dedicated least-privilege service-user, not root (except for plugins with `run_as_root`-capability). Protection against sidecar spoofing **together** with the binary rests precisely on the rights of the directory: an attacker without write access to the directory will not overwrite either the binary or `.sha256`. Sidecar `.sha256` is a digest cache for re-exec reconciliation (defense-in-depth shared cache), **not** trust-anchor: authoritative source "is the binary allowed" - Sigil (Keeper signature + allow-list `plugin_sigils`), and not a locally calculated hash.

### Closed gap — first-load

The previous TOFU model protected against binary substitution **after** the first loading into the cache, but **not** from a malicious plugin during the **first** loading (if the attacker replaced the binary in artifact-source / during Keeper-checkout of git-ref before the host saw it for the first time) - he went through integrity-gate "as is" and forked with service-user rights (RCE vector). **This gap is closed by Sigil** ([ADR-026](../adr/0026-sigil.md)): first-load is no longer "trust as is" - the host verifies the Keeper's signature and checks the digest with the value **to** seal/exec explicitly approved by the Archon. A Malicious binary without a valid Sigil does not receive control.

> **Implementation (host-side verify - LIVE).** Plugin directory Git resolver ready (A1-S1): [`keeper/internal/plugingit`](../../keeper/internal/plugingit) (go-git F-fetch, R-nested cache `<alias>/<commit_sha>/` + `current`, scheme-allowlist, git-egress size-limit (ADR-026(g)), sentinels `ErrRefNotResolved`/`ErrManifestNotFound`/`ErrArtifactNotFound`/`ErrSourceUnavailable`/`ErrCloneTooLarge`/`ErrArtifactTooLarge`), config fields `plugins.work_root`/`plugins.fetch_timeout`/`plugins.max_artifact_size_mb`/`plugins.max_clone_size_mb`; reading active slot at `plugin.allow` - [`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go) via `current`. Keeper-side signature is ready (S3): general block build helper + canonicalization manifest in [`shared/pluginhost`](../../shared/pluginhost) (`BuildSigilBlock` / `NormalizeManifestBytes`), ed25519-signature + CRUD registry `plugin_sigils` in [`keeper/internal/sigil`](../../keeper/internal/sigil), config `sigil.signing_key_ref`. `plugin.allow` persists the signed bytes in `schema` (M1-storage, migration 030, renamed and made NOT NULL by migration 113) - `ListActive` / `GetActive` give them to S6-sender/S6b-verify byte-exact. **Host-side verify-against-Sigil - LIVE (S6):** TOFU branch first-load is replaced by verify by Sigil + multi-anchor set in [`shared/pluginhost`](../../shared/pluginhost) (SHA-256 verification before each `exec` remains defense-in-depth). **Multi-anchor rotation of signature keys - LIVE (R3, [ADR-026(h)](../adr/0026-sigil.md)):** registry `sigil_signing_keys` (migration 037, [`keeper/internal/sigil/keys.go`](../../keeper/internal/sigil/keys.go)), multi-anchor Signer, broadcast `SigilTrustAnchors` + Redis channel `sigil:anchors-changed` (cluster reload), operator-facing rotation (R3-S7: REST `/v1/sigil/keys*` + MCP `keeper.sigil.key.*`, permissions `sigil.key-introduce|retire|list|set-primary`, audit `sigil.key-introduced|retired|primary-set`), bootstrap-reply from live anchor source. Deferred: column `commit_sha` in `plugin_sigils` (A1-S3, audit-label of origin, OUTSIDE signature).

## Versioning

`protocol_version: int` - plugin protocol version. **One field - two places** ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)):

- In the schema document - for static `soul-lint`.
- In the handshake line - for runtime sanity before opening gRPC.

### Match `protocol_version` ↔ `proto/plugin/vN/`

| `protocol_version` | proto package | Status | Composition |
|---|---|---|---|
| `1` | `proto/plugin/v1/` | MVP | `handshake.proto`, `manifest.proto`, `soulmodule.proto`, `clouddriver.proto`, `sshprovider.proto` (closing is a separate task after ADR-020). |

### `SupportedProtocolVersions`

Each host binary (`keeper` / `soul` / `soul-lint`) holds a constant - an ordered list of supported protocol versions. In MVP - `[1]`.

Evolution: adding `proto/plugin/v2/` → the next version of the host binary contains `[1, 2]` (forward-compat only-add, analogous to [ADR-012(g)](../adr/0012-keeper-soul-grpc.md) for the plugin protocol). Removing old versions - breaking release of the host binary, separate ADR.

### Cross-check matrix

Summary list of cross-checks between manifest, handshake string and host constant `SupportedProtocolVersions`. Duplicates runtime rows from the table ["Host behavior during handshake"](#host-behavior-during-handshake) in formal notation + adds static check `soul-lint`.

| fail condition | Where | Behavior |
|---|---|---|
| `document.protocol_version != handshake.protocol_version` | host, after handshake | Hard fail: drift inside the plugin. |
| `document.protocol_version ∉ SupportedProtocolVersions` | `soul-lint` when validating destiny (from a `--modules <alias>=<path>` binding) | Destiny validation error **before launch**. |
| `handshake.protocol_version ∉ SupportedProtocolVersions` | host, after handshake | Hard fail: `protocol_version=N, host supports [...]`. |
| `document.kind != handshake.kind` | host, after handshake | Hard fail: drift inside the plugin. |

## `required_capabilities`-table

Closed enum capabilities. The plugin declares what it needs from the host system. The **host** compares that declaration against `plugin_runtime.allowed_capabilities` at spawn and refuses to exec on a mismatch; `soul-lint` does **not** check capabilities at all, and nothing confines the process once it starts — see [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure).

| Capability | Meaning |
|---|---|
| `run_as_root` | The host process (`soul` / `keeper`) must have UID 0 when running the plugin. |
| `network_outbound` | The plugin makes outgoing network calls (cloud API, vault, package mirror). |
| `network_inbound` | The plugin listens to the port (a rare case for test helper plugins). |
| `vault_access` | The plugin accesses Vault through the client helper SDK. |
| `fs_write_root` | The plugin writes beyond `/var/lib/soul-stack/`. |
| `exec_subprocess` | The plugin runs external commands via `os/exec`. |

Enum expansion is done via PR in `proto/plugin/vN/manifest.proto`, without breaking. Freeform-extensions with the prefix `x-` (open-ended capabilities) **rejected in MVP** - will be added on the first real request.

`run_as_root` means "this module only works correctly when the host process is UID 0" — an environment requirement. It does **not** mean the module is raised to root: the step runs with exactly the privileges the host process (`soul` / `keeper`) already has, and no field grants any.

Built-in core modules (Soul-side, statically compiled) declare their capabilities as Go values in [`shared/coremanifest/mod_<name>.go`](../../shared/coremanifest). For them the field carries no runtime semantics either.

## Capabilities and side_effects are disclosure

Neither `capabilities` nor `side_effects` constrains what a plugin can do. Both are **disclosure to the operator before approval** ([ADR-020(r)](../adr/0020-plugin-infrastructure.md#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name)). This section is the correction to an earlier version of this page, which described enforcement the code does not have — worth stating flatly, because a reader who believes the old text will make a security decision on it.

**What is actually there:**

| Mechanism | Status |
|---|---|
| Sandbox at spawn confining the plugin to its declared capabilities | **Does not exist.** No `SysProcAttr`, no seccomp, no `Setuid`, no rlimit anywhere in `shared/pluginhost`. A plugin declaring `[]` and then opening a socket is stopped by nothing. |
| `soul-lint` statically checking `capabilities` ⊆ `allowed_capabilities` | **Does not exist.** `soul-lint` never reads capabilities. |
| Host check against `plugin_runtime.allowed_capabilities` | **Exists**, at spawn (`pluginhost.CheckCapabilities`). It compares strings and refuses to exec on a mismatch. With `allowed_capabilities` unset — the default — it is a no-op. It gates on the **declaration**, so it stops an honest plugin the operator did not want, not a dishonest one. |
| `side_effects` violation → step `failed`, reason `policy_violation` | **Does not exist.** No production code emits `policy_violation`; the string appears only in test fixtures. |
| Conflict detection between two plugins claiming one resource (`plugin_runtime.conflict_policy`) | **Does not exist.** `conflict_policy` is parsed and enum-validated in `shared/config` and read by nothing. |
| Audit event per touched resource | **Does not exist.** |

`side_effects` values are read in exactly one place in the tree — the grammar validator. Nothing consumes them.

**Why the fields stay.** `plugin.allow` is a human act: an Archon decides that a specific artifact may run with the service user's privileges, which on a Keeper host means access to Vault, Postgres and the PKI. What that artifact says it needs and what it says it will touch is exactly the material that decision is made on. Deleting the declarations would not make anything safer — it would remove the only thing the operator has to read. And under [Sigil](#integrity-model) the disclosure is signed, so what the operator approved is what the artifact actually claimed.

**The one real control is unchanged and is not one of these fields:** the operator approves a specific sha256, and the host refuses to exec anything whose digest differs. It consults no declaration, so no declaration being false can weaken it.

Making either field a control is a **separate ADR** with a real cost — enforcing `side_effects` means interposing on the plugin's syscalls, enforcing `capabilities` means an actual sandbox. Neither is designed, and neither should be assumed to exist because a schema key spells it.

## `side_effects`-table

Closed enum of resource types that the plugin touches (touched resources). The entry grammar is `{<resource-type>: <value>}`.

| Resource type | Value (type) | Example |
|---|---|---|
| `service` | `string` (service name) | `{ service: haproxy }` |
| `file` | `path` (absolute path) | `{ file: /etc/haproxy/haproxy.cfg }` |
| `package` | `string` (OS package name) | `{ package: haproxy }` |
| `port` | `int` (tcp/udp port) | `{ port: 80 }` |
| `user` | `string` (OS username) | `{ user: postgres }` |
| `group` | `string` (OS group name) | `{ group: postgres }` |
| `directory` | `path` (absolute directory path) | `{ directory: /var/lib/postgresql }` |
| `cron` | `string` (cron task name) | `{ cron: backup-nightly }` |
| `mount` | `path` (mountpoint) | `{ mount: /var/lib/data }` |

Enum extension - via PR in `proto/plugin/vN/`, without breaking. Wildcard values ​​(`file: /etc/haproxy/**`) and conditional `side_effects` (`when: …`) **rejected in MVP** - will be added during the first real request.

### Entry grammar `side_effects`

Each entry in `side_effects` is an object with **exactly one** `<resource_type>: <resource_value>` pair. If the plugin touches several resources of different types, these are **separate entries** in the list:

```yaml
side_effects:
  - { service: haproxy }
  - { file: /etc/haproxy/haproxy.cfg }
  - { port: 80 }
```

The validator rejects an entry with zero or more than one pair. Several resources of the **same** type are also separate entries (two different files → two `{ file: … }` entries).

In the Go declaration the resource type is a **struct field**, not a map key — `module.SideEffect{User: "redis_acl_user"}` — so a typo is a compile error in the author's own build rather than a validation error someone else finds later. It serializes to the same single-key object.

### Host behavior on side_effects

**The host does nothing with `side_effects` at run time.** No audit event, no conflict detection, no violation check — see [Capabilities and side_effects are disclosure](#capabilities-and-side_effects-are-disclosure) for the full list of what the previous version of this section claimed and what the code does. The values are read once, by the grammar validator, and never again.

The audience for this field is the operator reading the schema at `plugin.allow`.

## Service contract `SoulModule`

Host is the `soul` binary. The artifact is **the single executable in `dist/`** — its filename is not a contract ([Registration alias](#registration-alias)).

**The host selects a module by subcommand:** it forks `<artifact> <module>` — `soul-mod-redis acl` — and the process serves that one module for that one Apply. `ServeBundle` dispatches on the argument and also answers a `schema` subcommand, which is why `schema` is a reserved module name. This costs no proto change and does not move `protocol_version`: the host already forks per Apply ([ADR-020(d)](../adr/0020-plugin-infrastructure.md), one-shot), so multiplexing several modules over one socket would buy nothing.

| Method | Destination |
|---|---|
| `Validate(ValidateRequest) → ValidateReply` | Runtime parameter checks (the declared schema in `modules[].states.<state>.input` has already been checked; here are the additional semantic checks that need access to the host system). |
| `Plan(PlanRequest) → stream PlanEvent` | Dry-run: the module calculates changes without applying them. Returns the progress event stream. |
| `Apply(ApplyRequest) → stream ApplyEvent` | Applies the changes. Stream events for long-running operations (see the clause about MVP in [ADR-012](../adr/0012-keeper-soul-grpc.md) - progress is aggregated on Soul, only the final result goes out through `TaskEvent`). |

There is **no `Manifest()` RPC**, and now there is a second reason for it. The original one stands — `soul-lint` must validate a destiny offline, without starting the plugin ([ADR-009](../adr/0009-scenario-dsl.md)). The stronger one is the approval order: Keeper reads the schema at `plugin.allow`, when the binary is **not yet approved**, so asking the artifact to describe itself would mean executing the thing being gated. The schema comes from the trailer instead.

Destiny step addressing is `<namespace>.<name>.<state>` (see [`../soul/modules.md`](../soul/modules.md), [naming-rules.md → Destiny Modules](../naming-rules.md)).

## Service contract `CloudDriver`

> ⚠ **This contract is being deleted — epic NIM-757, decided 2026-09-01, NOT implemented.**
> `proto/plugin/v1/clouddriver.proto`, its committed generated Go and the `sdk/clouddriver/` module directory go
> (Option A — no backward-compatibility branch, no `proto/plugin/v2`: nothing is changing shape, it is going
> away). An already-built third-party driver binary is not broken — the wire is untouched — it simply stops
> being called once `keeper/internal/pluginhost/clouddriver.go` goes; what breaks is a **rebuild** against a
> newer tag. A cloud driver's replacement is an ordinary SoulModule plugin declaring `side: keeper`
> ([ADR-017 amendment 2026-09-01](../adr/0017-keeper-side-core.md#amendment-2026-09-01-nim-757-the-clouddriver-contract-is-removed--a-cloud-driver-is-an-ordinary-plugin)).
> **The contract below is live and the six official drivers implement it.** (⚠ The method table is also short one
> row: `service CloudDriver` carries **seven** RPCs — `Resize` is missing here. Left as-is deliberately; the
> contract is going away and a correction would be churn.)

Host - `keeper` (module `keeper.cloud`, see [`cloud.md`](cloud.md)). The artifact is the single executable in `dist/`; repositories conventionally name it `soul-cloud-<provider>`, and nothing reads that name.

| Method | Destination |
|---|---|
| `Schema(SchemaRequest) → SchemaReply` | Publishes `profile_schema` (JSON Schema of the VM profile; must match the schema document's root `profile_schema`). Used when creating a Profile via OpenAPI/MCP for validation. `SchemaRequest` is an empty message (instead of `google.protobuf.Empty`) for forward-compat to add fields without breaking change. |
| `Validate(ValidateProfileRequest) → ValidateProfileReply` | Runtime checks of profile parameters (quotas, image availability, subnet validity - things that are not expressed by JSON Schema). The request/reply name is different from SoulModule `ValidateRequest/Reply` - a single proto-package `soulstack.plugin.v1`, message names must be unique. |
| `Create(CreateRequest) → stream CreateEvent` | Creates a VM (one or N), streams progress. The closing wait-until-ready phase runs on its own budget (`clouddriver.DefaultWaitBackoff`, `SOUL_CLOUD_WAIT_BUDGET`), not on the API-retry backoff - see [cloud.md → Wait-until-ready budget](cloud.md#wait-until-ready-budget). |
| `Destroy(DestroyRequest) → stream DestroyEvent` | Deletes a VM and CONFIRMS it is gone (`clouddriver.ConfirmDestroy`): an accepted delete call is not a deletion, so a teardown the driver could not confirm is reported `failed=true` with the `vm_id`, never as a success - see [cloud.md → Confirmed teardown](cloud.md#confirmed-teardown). Under guard-rails - see [cloud.md → Security destroy](cloud.md). |
| `Status(StatusRequest) → StatusReply` | Poll the status of a specific VM. `StatusRequest.credentials` carries the plain-secret of the provider (A-flow, symmetrically `CreateRequest`/`DestroyRequest`) - without credentials the driver will not be able to access the provider API. |
| `List(ListRequest) → stream VmInfo` | Enumeration of VMs known to the provider. `ListRequest.credentials` - A-flow (symmetrical `Create`/`Destroy`/`Status`); `ListRequest.filter` - provider-specific filter (tags/region), credentials CANNOT be placed in filter. |

Usage - in [`cloud.md`](cloud.md). Cloud-create is built into scenarios as a step addressing `core.cloud.provisioned` ([ADR-017](../adr/0017-keeper-side-core.md)). The step is routed to the Keeper by that address alone — `core.cloud` is one of the seven keeper-side core addresses — and writing `on: keeper` beside it is refused as `on_keeper_redundant` ([ADR-0087](../adr/0087-task-side-derived-from-module-address.md)).

## Service contract `SshProvider`

Host - `keeper` (module `keeper.push`, see [`push.md`](push.md)). The artifact is the single executable in `dist/`; repositories conventionally name it `soul-ssh-<provider>`, and nothing reads that name.

| Method | Destination |
|---|---|
| `Sign(SignRequest) → SignReply` | Issue an SSH certificate/key for the current session (for example, Vault SSH CA issues a short-lived certificate). |
| `Authorize(AuthorizeRequest) → AuthorizeReply` | Confirm Keeper's right to go to a specific host (provider policy, if any). |

This contract covers Vault SSH CA, static-key, Teleport - three candidates for MVP, a specific set of required implementations - [open Q SSH-2 / No. 3](../architecture.md). Usage is in [`push.md → SSH authentication`](push.md#ssh-authentication--pluggable-provider).

## Full examples

**Nobody writes these documents.** For `kind: soul_module` the author writes Go and the document is generated; the JSON below is shown so you can recognize what `soul-mod stamp` produced and what an operator reads at `plugin.allow`. Note that the artifact names no publisher and no subject — only its modules.

### `kind: soul_module` (HAProxy)

What the author writes:

```go
// internal/haproxy/haproxy.go
var Module = module.Def{
	Name:        "haproxy",
	Description: "HAProxy service and configuration",
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
		"stopped": {
			Description: "HAProxy is stopped",
			Input:       module.Input{"name": {Type: module.String, Required: true}},
		},
		"reloaded": {
			Description: "HAProxy reloaded (SIGHUP), without downtime",
			Input:       module.Input{"name": {Type: module.String, Required: true}},
		},
	},
}
```

```go
// cmd/soul-mod-haproxy/main.go
func main() {
	module.ServeBundle(module.Bundle{
		Compat:  module.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []module.Def{haproxy.Module},
	})
}
```

What `soul-mod stamp` generates (shown indented for reading; the real document is canonical JSON with sorted keys and no insignificant whitespace):

```json
{
  "kind": "soul_module",
  "protocol_version": 1,
  "compat": { "keeper": ">=0.9 <2.0" },
  "modules": [
    {
      "name": "haproxy",
      "description": "HAProxy service and configuration",
      "capabilities": ["run_as_root", "exec_subprocess"],
      "side_effects": [
        { "service": "haproxy" },
        { "file": "/etc/haproxy/haproxy.cfg" },
        { "package": "haproxy" }
      ],
      "states": {
        "running": {
          "description": "HAProxy is running and enabled in systemd",
          "input": {
            "name":        { "type": "string", "required": true },
            "enabled":     { "type": "bool", "default": true },
            "config_path": { "type": "string", "default": "/etc/haproxy/haproxy.cfg" }
          }
        },
        "stopped": {
          "description": "HAProxy is stopped",
          "input": { "name": { "type": "string", "required": true } }
        },
        "reloaded": {
          "description": "HAProxy reloaded (SIGHUP), without downtime",
          "input": { "name": { "type": "string", "required": true } }
        }
      }
    }
  ]
}
```

Registered as `acme`, this artifact answers `acme.haproxy.running`; registered as `haproxy-community`, the same bytes answer `haproxy-community.haproxy.running`.

### `kind: soul_module` (a bundle: redis)

One artifact, three modules — the shape NIM-377 exists for:

```json
{
  "kind": "soul_module",
  "protocol_version": 1,
  "compat": { "keeper": ">=0.9 <2.0" },
  "modules": [
    { "name": "acl",    "capabilities": ["network_outbound"],
      "side_effects": [{ "user": "redis_acl_user" }],
      "states": { "present": { "input": { "host": { "type": "string", "required": true } } },
                  "absent":  { "input": { "host": { "type": "string", "required": true } } } } },
    { "name": "config", "capabilities": ["network_outbound"], "states": { "present": {} } },
    { "name": "info",   "capabilities": ["network_outbound"], "states": { "read": {} } }
  ]
}
```

Registered as `redis`, it serves `redis.acl.present`, `redis.config.present` and `redis.info.read`. The host forks `<artifact> acl` for the first of those.

### `kind: cloud_driver` (AWS)

```json
{
  "kind": "cloud_driver",
  "protocol_version": 1,
  "provider_kind": "aws",
  "profile_schema": {
    "type": "object",
    "required": ["image_id", "instance_type", "subnet_id"],
    "properties": {
      "image_id":      { "type": "string", "pattern": "^ami-[0-9a-f]+$" },
      "instance_type": { "type": "string" },
      "subnet_id":     { "type": "string", "pattern": "^subnet-[0-9a-f]+$" },
      "security_group_ids": {
        "type": "array",
        "items": { "type": "string", "pattern": "^sg-[0-9a-f]+$" }
      },
      "tags": { "type": "object", "additionalProperties": { "type": "string" } }
    }
  }
}
```

A `cloud_driver` has no `modules[]` and no `side_effects`: it touches nothing on the local host. Its `capabilities` (`network_outbound`, `vault_access`) are declared per module and a driver has none — the endpoint-shaped kinds carry no capability block, which is one of the loose ends listed under [Which root field belongs to which kind](#which-root-field-belongs-to-which-kind).

### `kind: ssh_provider` (Vault SSH CA)

The actual implementation is [`examples/module/soul-ssh-vault/`](../../examples/module/soul-ssh-vault). Vault SSH CA: the plugin goes to Vault itself (variant B, see below), calls `ssh/sign/<role>` to sign Keeper-ephemeral pubkey, returns only `certificate` (`private_key=""`).

**Canonical mode - Keeper-ephemeral** (security-first, PM-decision):

1. `keeper.push` generates an ephemeral ed25519 keypair per-session and passes the public part to `SignRequest.public_key`.
2. The plugin authenticates to Vault (`auth_method: token | approle`), calls `<vault_mount>/sign/<role>` with this pubkey and `valid_principals=<req.user>`.
3. Returns `SignReply{certificate=<signed_key>, private_key=""}` - the private NEVER leaves Keeper.
4. `keeper.push` collects [`ssh.NewCertSigner`](https://pkg.go.dev/golang.org/x/crypto/ssh#NewCertSigner) from ephemeral signer + cert and opens an SSH session. After the session is closed, the private person goes to the GC.

**Creds-flow - variant B** (the plugin itself runs in Vault, [`vault_access` capability](#required_capabilities-table)): for `ssh/sign` this is more natural than A-flow (Keeper does not act as a proxy to the Vault engine, does not parse the response). Symmetrically for `auth_method: approle` the plugin does `auth/<mount>/login` itself.

**Params** come via env `SOUL_SSH_VAULT_PARAMS` (JSON by `schema.json`, symmetrically by `SOUL_SSH_STATIC_PARAMS`). The SshProvider contract does not carry per-request provider parameters, so the config is sent at the start of the process, like the path to the socket (`SOUL_PLUGIN_SOCKET`).

```yaml
# schema document for soul-ssh-vault (generated; shown as YAML for readability)
kind: ssh_provider
protocol_version: 1

provider_kind: vault_ssh_ca
params_schema:
  type: object
  required: [vault_addr, role]
  properties:
    vault_addr:  { type: string, pattern: "^https?://" }
    vault_mount: { type: string, default: "ssh" }
    role:        { type: string }
    auth_method: { type: string, enum: [token, approle], default: token }
    token:       { type: string }                    # SENSITIVE; for auth_method=token
    approle:
      type: object
      properties:
        role_id:   { type: string }
        secret_id: { type: string }                  # SENSITIVE
        mount:     { type: string, default: "approle" }
    valid_principals:                                # local allowlist over Vault role
      type: array
      items: { type: string }
    deny:                                            # deny-list paras (host, user); empty = allow-all
      type: array
      items:
        type: object
        properties:
          host: { type: string }
          user: { type: string }
```

> The old manifest declared `required_capabilities: [network_outbound, vault_access]`. The schema document declares capabilities **per module**, and this kind has no `modules[]` — see [Which root field belongs to which kind](#which-root-field-belongs-to-which-kind).

Example `SOUL_SSH_VAULT_PARAMS` (JSON, passed to the plugin env during fork):

```json
{
  "vault_addr": "https://vault.internal:8200",
  "vault_mount": "ssh",
  "role": "keeper-push",
  "auth_method": "approle",
  "approle": { "role_id": "...", "secret_id": "...", "mount": "approle" },
  "valid_principals": ["soul", "deploy"],
  "deny": [{ "user": "root" }]
}
```

### `kind: ssh_provider` (static-key)

Reference implementation of SshProvider (circulation pilot) - [`examples/module/soul-ssh-static/`](../../examples/module/soul-ssh-static). Static-key: long-lived private key on the keeper host, its public part is in `authorized_keys` target hosts ([push.md → static key](push.md)). `Sign` gives a ready pair (`certificate=""`), `Authorize` — deny-list (default allow-all, for dev/test). Params of the provider (`key_path` / deny-list) arrive at the start via env (SshProvider contract does not carry per-request parameters of the provider; `vault_ref` resolves `keeper.push` to `key_path` before launching the plugin - A-flow, parallel with cloud credentials).

```yaml
# schema document for soul-ssh-static (generated; shown as YAML for readability)
kind: ssh_provider
protocol_version: 1

provider_kind: static_key
params_schema:
  type: object
  oneOf:                  # exactly one key source
    - required: [key_path]
    - required: [vault_ref]
  properties:
    key_path:  { type: string }
    vault_ref: { type: string }
    deny:                 # deny-list paras (host, user); empty = allow-all
      type: array
      items:
        type: object
        properties:
          host: { type: string }
          user: { type: string }
```

> The old manifest declared `required_capabilities: [vault_access]`. The schema document declares capabilities **per module**, and this kind has no `modules[]` — see [Which root field belongs to which kind](#which-root-field-belongs-to-which-kind).

### `kind: ssh_provider` (Teleport)

The actual implementation is [`examples/module/soul-ssh-teleport/`](../../examples/module/soul-ssh-teleport). Teleport provider: the plugin goes to Teleport Auth itself (creds-flow B, symmetrically to Vault SSH CA), calls `GenerateUserCerts(SSHPublicKey)` to sign the Keeper-ephemeral pubkey, returns only `certificate` (`private_key=""`) and fills in the only-add field with `SignReply.proxy_jump` endpoint of the Teleport-proxy.

**Canonical mode - Keeper-ephemeral** (security-first, PM-decision): Teleport API (`api/client.GenerateUserCerts`) accepts `SSHPublicKey []byte` and returns a signed SSH-cert for this pubkey - variant A (Vault-style) fits into Teleport without deviating from the key-ownership solution.

**Creds-flow - variant B** (the plugin itself authenticates itself in Teleport): identity-file (`tctl auth sign`) or tbot socket, the path comes to `SOUL_SSH_TELEPORT_PARAMS`. Capability `vault_access` NOT required - Teleport is not Vault; credentials live in a file/socket.

**Dispatcher proxy_jump support - LIVE (S3).** `keeper.push` respects `SignReply.proxy_jump`: if the value is non-empty, the dispatcher opens the SSH client to the proxy hop with the same `cfg.Auth` (signed user-cert on the ephemeral keypair), on this client requests the `direct-tcpip` channel before `host:port` of the target host and performs a second SSH handshake over the channel (equivalent to `ssh -J <proxy> <host>`). Cert from Teleport/Vault SSH CA authorizes the user on both hops - this is canonical Teleport-flow. Host-cert verification (`ssh.CertChecker`, fail-closed without CA) works on BOTH hops: by default, one `HostAuthority.CAPublicKey` is used (typical case - one host-CA signs both proxy and target host-certs); separate proxy-CA - extension via `DialConfig.ProxyHostAuthority` (field without UI for now, activated upon operator request). If `proxy_jump` is empty - direct `net.Dial(host:port)` (S0-flow without regressions). Implementation - [`keeper/internal/push/session.go`](../../keeper/internal/push/session.go) (function `Dial`, branch `dialViaProxy`).

**Params** come via env `SOUL_SSH_TELEPORT_PARAMS` (JSON by `schema.json`, symmetrically `SOUL_SSH_VAULT_PARAMS` / `SOUL_SSH_STATIC_PARAMS`).

```yaml
# schema document for soul-ssh-teleport (generated; shown as YAML for readability)
kind: ssh_provider
protocol_version: 1

provider_kind: teleport
params_schema:
  type: object
  required: [proxy_addr]
  properties:
    proxy_addr:    { type: string }            # Teleport proxy host:port (goes to SignReply.proxy_jump)
    cluster_name:  { type: string }            # multi-cluster trust (optional)
    identity_file: { type: string }            # path to Teleport identity-file (creds-flow B)
    tbot_socket:   { type: string }            # or tbot socket (mutually exclusive with identity_file)
    roles:                                     # requested Teleport roles (optional)
      type: array
      items: { type: string }
    valid_principals:                          # local allowlist over Teleport role
      type: array
      items: { type: string }
    deny:                                      # deny-list paras (host, user); empty = allow-all
      type: array
      items:
        type: object
        properties:
          host: { type: string }
          user: { type: string }
```

> The old manifest declared `required_capabilities: [network_outbound]`. The schema document declares capabilities **per module**, and this kind has no `modules[]` — see [Which root field belongs to which kind](#which-root-field-belongs-to-which-kind).

Example `SOUL_SSH_TELEPORT_PARAMS` (JSON, passed to the plugin env during fork):

```json
{
  "proxy_addr": "teleport.example.com:3023",
  "cluster_name": "root",
  "identity_file": "/etc/teleport/keeper-push.identity",
  "roles": ["node-admin"],
  "valid_principals": ["soul", "deploy"],
  "deny": [{ "user": "root" }]
}
```

### `kind: soul_beacon` (ZFS pool health, ADR-030 V5-2)

```yaml
# schema document (generated; shown as YAML for readability)
kind: soul_beacon
protocol_version: 1

params_schema:
  type: object
  required: [pool]
  properties:
    pool: { type: string }    # ZFS pool name to poll
```

> The old manifest declared `required_capabilities: [exec_subprocess]` and `side_effects: []`. The schema document declares both **per module**, and this kind has no `modules[]` — see [Which root field belongs to which kind](#which-root-field-belongs-to-which-kind). The Vigil address of a plugin beacon is level-1 the registration alias, level-2 the beacon, exactly as for modules.

SDK - [`sdk/beacon`](../../sdk/beacon/beacon.go). Minimum plugin code:

```go
package main

import (
    "context"

    pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
    "github.com/souls-guild/soul-stack/sdk/beacon"
    "google.golang.org/protobuf/types/known/structpb"
)

type ZFSDegraded struct { beacon.BaseBeacon }

func (z *ZFSDegraded) Check(_ context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
    pool := req.GetParams().GetFields()["pool"].GetStringValue()
    state := "ok"
    if poolIsDegraded(pool) {
        state = "degraded"
    }
    payload, _ := structpb.NewStruct(map[string]any{"pool": pool})
    return &pluginv1.CheckReply{State: state, Payload: payload}, nil
}

func main() { beacon.Serve(&ZFSDegraded{}) }
```

`Check` called by Soul-scheduler per-tick (interval from `VigilDef.interval`); change `state` → `PortentEvent.payload.custom` ([V5-1 typed payload](../module/core/beacon/README.md#typed-portentpayload-v5-1)).

## Plugin directory in `keeper.yml`

Declared in the `plugins:` block ([config.md](config.md)):

```yaml
plugins:
  cloud_drivers:
    - { name: aws, source: "git@github.com:soul-stack-ecosystem/soul-cloud-aws.git", ref: v2.0.0 }
    - { name: yc,  source: "git@github.com:our-company/soul-cloud-yc.git",          ref: v0.3.1 }

  ssh_providers:
    - { name: vault-ssh, source: "git@github.com:soul-stack-ecosystem/soul-ssh-vault.git", ref: v1.0.0 }
    - { name: static,    source: "git@github.com:soul-stack-ecosystem/soul-ssh-static.git", ref: main }

  soul_modules:                     # SoulModule plugins (ADR-065): resolved by the same resolver, allowed by the same Sigil flow
    - { name: redis, source: "git@github.com:souls-guild/soul-mod-community-redis.git", ref: v1.2.0 }
```

Plugin version is **git ref** (tag or branch) according to [ADR-007](../adr/0007-versioning-git-ref.md). No semver-range.

**`name:` in a catalog entry is the registration alias** — the operator's choice, and address level 1 for everything the artifact serves ([Registration alias](#registration-alias)). It is not read from the artifact, and the artifact has no opinion about it: registering the same `source`+`ref` twice under two aliases gives two slots and two addresses. Aliases from the [reserved list](../naming-rules.md#reserved-namespace-names) are refused.

### What Keeper does as a resolver (git-verified, F-fetch)

Keeper resolves the directory itself at startup - via [`keeper/internal/plugingit`](../../keeper/internal/plugingit) ([ADR-026(g)](../adr/0026-sigil.md), A1-S1). For each entry:

1. `validateGitScheme(source)` — scheme-allowlist: prod `https://` / `ssh://` / scp-form `user@host:path`; `file://` - only under the env flag `SOUL_STACK_ALLOW_FILE_REPOS=1` (dev/test). Different scheme → `ErrSourceUnavailable`.
2. shallow `clone` (`Depth=1`) working clone in `<work_root>/<name>/` (STRICTLY outside `cache_root`), or `fetch` if there is already a clone. Transport - **go-git** (pure-Go, without system fork `git`); auth - SSH agent for ssh/scp forms.
3. `ResolveRevision(<ref>^{commit})` → 40-hex `commit_sha` (candidates: tag → remote-tracking-branch → full hash; unresolved → `ErrRefNotResolved`).
4. detached-HEAD `checkout` on `commit_sha` (does not execute go-git hooks).
5. take **the single executable in `dist/`** (the binary is already built, **F-fetch — Keeper does not compile**) and read the schema document from its trailer **without executing it**. Zero executables, several of them, a non-regular file, or a missing / malformed trailer → fail-closed for this entry (the exact sentinel set is fixed by the resolver slice; today's `ErrArtifactNotFound` covers the not-found case).
6. atomic-extract schema+binary into the immutable slot `<cache_root>/<alias>/<commit_sha>/` (staging on the same fs → fsync → `rename`); the `commit_sha` slot is immutable (re-resolving the same commit — skip).
7. atomic switching symlink `<cache_root>/<alias>/current → <commit_sha>`.
8. `binary_sha256 := sha256(<the executable in the slot>)`.

Per-entry resolve **fail-closed**: broken entry (any sentinel above / unreachable remote / timeout) → per-entry warning, Keeper does not crash. During apply operations, the plugin is launched from the active slot (`current`).

git stack - go-git by-design: hooks are not executed, submodules are not recursive, `ext::` is missing, `file://` is locked by scheme-allowlist; **no runtime dependency on the `git`** binary on keeper-host. Hardening: `Depth=1` + context-timeout (`plugins.fetch_timeout`, default 120s), `plugins.work_root` STRICTLY outside `cache_root`, **size-limit by volume** (`plugins.max_clone_size_mb` for the clone working tree + `plugins.max_artifact_size_mb` for the binary, fail-closed - see below). git-egress - **HIGH security risk**: the remainder of the mandatory security pass before proceeding (`noexec` per slot / sandbox of git operations) - postponed. **GC of old `commit_sha`-slots** (several slots per pair after `ref` rotations/commits) - deferred, candidate for Reaper rule (name - separate propose-and-wait).

**Size-limit (ADR-026(g), fail-closed).** `source` operator-asserted, but the repository is untrusted, and `fetch_timeout` limits egress only in time. Two caps in size protect the keeper-host disk from DoS by a hostile/huge repo:

- `plugins.max_clone_size_mb` (default 1024 MiB) - the total size of the clone working tree (du-like walk checkout + `.git`), checked **after checkout, before extracting the artifact**. Excess → `ErrCloneTooLarge` + cleanup `work_root/<name>`.
- `plugins.max_artifact_size_mb` (default 256 MiB) - size of the executable in `dist/`, checked against `os.Stat` before copying and `io.LimitReader` during copy (defense-in-depth). Excess → `ErrArtifactTooLarge`, slot does not materialize.

Both sentinels are per-entry fail-closed (warning, like `ErrArtifactNotFound`/`ErrSourceUnavailable`): the broken entry is skipped, the slot is not created → the plugin has **nothing to allow** through Sigil.

Resolver Config fields in [`config.md → plugins`](config.md):

| Field | Default | Meaning |
|---|---|---|
| `plugins.cache_root` | `pluginhost.DefaultCacheRoot` | Root of the R-nested slot cache (`<alias>/<commit_sha>/`). Absolute path. |
| `plugins.work_root` | `/var/lib/soul-stack-keeper/plugin-src` | The root of the resolver's working git clones. **STRICTLY outside `cache_root`** (`.git`/checkout does not go into the readable cache directory). Absolute path. |
| `plugins.fetch_timeout` | `120s` | The ceiling of one chain of go-git resolve operations (clone→fetch→checkout). git-egress - external call, timeout required. |
| `plugins.max_artifact_size_mb` | `256` | Size ceiling of the executable in `dist/` (size-limit hardening). Excess → `ErrArtifactTooLarge`, fail-closed. |
| `plugins.max_clone_size_mb` | `1024` | Clone working tree size ceiling (checkout + `.git`). Excess → `ErrCloneTooLarge` + cleanup, fail-closed. |

Directory of `SoulModule` plugins - **`plugins.soul_modules[]`** in the same format (`{name, source, ref}`; [ADR-065](../adr/0065-core-module-installed.md), amendment [ADR-020](../adr/0020-plugin-infrastructure.md)): resolved by the same resolver in `cache_root`, allowed by the same Sigil flow. Distribution to Soul hosts - server-streaming RPC `FetchModule` (content-addressed: Keeper distributes only bytes whose sha256 is in the active permission `kind: soul_module`) + core module `core.module.installed` (see [`../soul/modules.md`](../soul/modules.md)). Install steps `core.module.installed` Keeper usually synthesizes itself from `service.yml::modules[]` ([keeper/modules.md → Auto-synthesis](modules.md)).

## Benefits of a single infrastructure

- One SDK per language (Go / Rust / Python) covers all three kinds of plugins through a common `sdk/handshake/` helper.
- One method of distribution and caching: git resolve of the `plugins.*` directory into the Keeper cache + host cache via SHA-256 (on Soul - via `FetchModule`, [ADR-065](../adr/0065-core-module-installed.md)) + Sigil verification before launch (see. [Integrity-model](#integrity-model)).
- One configuration method (manifest + JSON Schema parameters).
- Third parties can release their own plugins (cloud provider for a niche cloud, SSH provider for a non-standard CA, custom module for a specific company) without modifying the Soul Stack core.
- One audit-trail format (`side_effects` → audit-event).

## See also

- [architecture.md → ADR-020](../adr/0020-plugin-infrastructure.md) - regulatory decision for this entire document.
- [architecture.md → Plugin infrastructure](../architecture.md) - high-level overview.
- [`../soul/modules.md`](../soul/modules.md) - `SoulModule`-specifics, layout on the Soul host, cache.
- [cloud.md](cloud.md) - use of `CloudDriver`.
- [push.md](push.md) - use of `SshProvider`.
- [config.md](config.md) → `plugins:` - directory format.
- [architecture.md → ADR-007](../adr/0007-versioning-git-ref.md) - `ref:` as a plugin version, exception for `protocol_version`.
- [architecture.md → ADR-011](../adr/0011-go-layout.md) - `proto/plugin/` submodule and `sdk/handshake/`.
- [architecture.md → ADR-016](../adr/0016-parity-license.md) - why is it not dependent on `hashicorp/go-plugin`.
- [naming-rules.md](../naming-rules.md) — `Schema document`, `Registration alias`, `Handshake`, `kind`, `SoulModule`, `CloudDriver`, `SshProvider`, capabilities-enum, resource-types-enum.
- [naming-rules.md → Reserved namespace names](../naming-rules.md#reserved-namespace-names) — the closed list an alias may not be taken from.
- [module-collections.md](../module-collections.md) — the collection as an entity; level 1 is now the registration alias.
