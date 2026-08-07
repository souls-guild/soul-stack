# ADR-020. Plugin Infrastructure: manifest format, handshake, lifecycle

- **Context.** Soul Stack has three categories of plugins with different service contracts (`SoulModule`, `CloudDriver`, `SshProvider`), but a **single infrastructure** — handshake, launch method, manifest format, versioning (see the ["Plugin infrastructure"](../architecture.md#plugin-infrastructure) section, [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)). At the time of this fixation, this infrastructure was only described in sketch form:
  - [§"Module schema document"](../architecture.md#module-schema-document) is given as a draft example, with an explicit open sub-Q of "a separate `manifest.yaml` next to the binary vs the first RPC method `Manifest()`".
  - [§"Module protocol — gRPC over stdio (HashiCorp-style)"](../architecture.md#module-protocol---grpc-over-stdio-hashicorp-style) — a general reference to the HashiCorp `go-plugin` model, with an open sub-Q of "file name and exact protocol version".
  - The handshake-string format, the list of `required_capabilities`, the `side_effects` grammar, plugin lifecycle, the socket path, and the TLS policy are nowhere normatively fixed.

  Without a normative fixation, we can neither finalize `proto/plugin/v1/*.proto` (the next task after this ADR), nor write a unified handshake helper in `sdk/handshake/`, nor implement static destiny validation in `soul-lint` (the latter being a requirement of [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)).

- **Decision.**

  > **(a) is SUPERSEDED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377).** There is no hand-written `manifest.yaml`. The source of truth is a `module.Def` value in Go; the schema document is generated from it and stamped into the artifact. The requirement below — parse it without running the binary — is unchanged and is what the stamping mechanism exists to satisfy.

  **(a) Manifest — a static `manifest.yaml` in the plugin's repo.** The file sits at the root of the plugin's repository and ships alongside the binary (in Keeper's cache / on the Soul host). `soul-lint` parses it **without running the binary** — this is a direct requirement of [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) (static validation of destiny without bringing up the plugin process).

  The alternative "RPC-only via a `Manifest()` method" is rejected: it breaks `soul-lint`'s offline validation (the plugin would need to be downloaded and started first). The "hybrid" alternative is rejected as over-engineering with no benefit.

  Drift "the manifest is out of sync with the plugin's actual code" is a real risk; it is mitigated via a plugin self-test (calling `Apply` with parameters outside the input schema must return `INVALID_ARGUMENT`) and an optional generated manifest (`soul-mod gen-manifest --check`) — an SDK extension post-MVP, without breaking changes.

  **(b) Handshake — a single-line JSON with a magic prefix field.** On startup the plugin writes **exactly one line** to stdout:

  ```json
  {"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/acme-haproxy-12345.sock","server_cert":""}
  ```

  - The magic field `"soul_stack":"plugin-v1"` is a sanity check. The host ignores all stdout lines before the first line with this field (protection against an accidental `fmt.Println` in the plugin's `init()` or output from libraries).
  - Fields: `soul_stack` (a constant string, `"plugin-v1"`), `protocol_version` (int), `kind` (an enum, see (e)), `network` (an enum `unix`, extended to `named_pipe` / `tcp` post-MVP), `address` (the socket path), `server_cert` (base64-PEM or an empty string; reserved for optional post-MVP mTLS, see (h)).
  - Extension via new **optional** keys (`features`, `capabilities`, …) — without breaking changes.
  - **We do not use the `hashicorp/go-plugin` library as a dependency.** Its 6-field pipe-string format is excessive and inflexible; the MPL-2.0 copyleft is not needed (see [ADR-016](0016-parity-license.md)). Only the "one-line handshake → gRPC-over-socket" model is borrowed.

  Rejected: (a) the HashiCorp 6-field pipe string (excessive fields + a rigid format), (b) a minimal pipe (extension = breaking), (c) framed exchange (unreadable, doesn't fit the one-line-handshake model).

  **(c) Versioning — `protocol_version` is duplicated in the manifest and in the handshake.** One int, two places:
  - In `manifest.yaml` — for static `soul-lint` (without running the plugin).
  - In the JSON handshake string — for a runtime sanity check **before** opening the gRPC channel.

  The host (`keeper` / `soul` / `soul-lint`) holds the constant `SupportedProtocolVersions = [1]`. Three cross-checks:
  - `handshake.protocol_version != manifest.protocol_version` → the plugin is invalid, refuse to start (drift inside the plugin).
  - `protocol_version` outside `SupportedProtocolVersions` → a hard fail with the message `protocol_version=N, host supports [...]`.
  - `manifest.kind != handshake.kind` → refuse to start (drift).

  A strict correspondence between **`protocol_version: N` ↔ `proto/plugin/vN/`** (one go.mod submodule per [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules)). Evolution: adding `proto/plugin/v2/` → a host of version N+1 supports `[1, 2]` (forward-compat only-add, the plugin-protocol analog of [ADR-012(g)](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add); never delete/reuse field numbers in `proto/plugin/v1/`).

  `protocol_version` is a **compat API flag, not an artifact version**. This is the exception for `protocol_version` already articulated in [ADR-007](0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field); ADR-020 does not introduce a new exception, it only fixes its place within the plugin infrastructure.

  Rejected: (a) handshake-only / (d) gRPC reflection — both break [ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation) (offline validation); (b) manifest-only — drift is not caught.

  > **(d) is AMENDED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377):** one artifact now serves several modules, and the host selects which one by passing it as the **first argument** — `soul-mod-redis acl`. The one-shot lifecycle fixed below is what makes this free: the host already forks per Apply, so a long-lived process multiplexing modules over the socket would buy nothing. Everything else in (d) — socket type, path, startup, shutdown, timeouts — is unchanged, and `protocol_version` does not move.

  **(d) Socket + lifecycle.**
  - **Socket type:** Unix domain socket only in the MVP. The `network` field in the handshake JSON allows extending the enum (`unix | named_pipe | tcp`) — Windows support post-MVP without breaking changes.
  - **Socket path:** the host passes it via the env var **`SOUL_PLUGIN_SOCKET`**. The directory is `/var/run/soul-stack/plugins/<namespace>-<name>-<pid>.sock` for a Soul host, `/var/run/soul-stack-keeper/plugins/<namespace>-<name>-<pid>.sock` for a Keeper host; mode `0700`, owned by the service user (`soul` or `keeper`). The SDK trivially reads the env var and opens the socket.
  - **Startup:** the host forks the plugin process, passes `SOUL_PLUGIN_SOCKET`, reads stdout until the first line with `"soul_stack":"plugin-v1"`. All lines before that are ignored (but logged at debug level).
  - **Shutdown:** the host sends SIGTERM; the SDK provides a signal-handler helper, the plugin finishes its current RPCs and exits. A `Shutdown()` RPC is not introduced into the MVP proto contract (an extension in `proto/plugin/v2/` if needed). Grace period 10s — if the plugin has not exited — SIGKILL.
  - **Lifecycle:** **one-shot** — started for each Apply, exits afterward. Long-lived (one process for a series of calls) — a separate ADR if needed (profiling shows a cold-start cost, or a CloudDriver with batched operations against a single cloud-API token appears).
  - **Timeouts (defaults):** startup `10s` (the handshake string must appear), shutdown grace `10s` (SIGTERM → SIGKILL). Specific values are configured via the `keeper.yml` / `soul.yml` block `plugin_runtime:` — the normative specification is in [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) and [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime).

  > **(e) is SUPERSEDED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377)** in three respects: the document is generated canonical JSON rather than hand-written YAML; `namespace:` and `name:` are **gone** (the artifact carries no self-identity); and the binary-name convention is gone entirely. What survives: one document shape for every kind, discriminated by `kind:`, with kind-specific content under `spec:`.
  >
  > The paragraph below calling the binary name "a convention, not a contract — a mismatch warns, does not fail" **never matched the code**: `pluginhost.DiscoverSlot` computed `Manifest.BinaryName()` and skipped the whole slot when no file of that name sat beside the document, so a mismatch failed silently rather than warning. That contradiction is now moot — the convention has no successor to be wrong about — but it is recorded rather than deleted, because the drift stood for the entire life of the decision and a reader tracing NIM-423 will end up here.

  **(e) Manifest format — a single schema with a `kind:` discriminator.** The same YAML format for all three plugin types; differences live in the `spec:` section:

  ```yaml
  # soul-mod-haproxy/manifest.yaml
  kind: soul_module                 # discriminator: soul_module | cloud_driver | ssh_provider
  protocol_version: 1
  namespace: acme
  name: haproxy
  required_capabilities: [run_as_root]
  side_effects:
    - { service: haproxy }
    - { file: /etc/haproxy/haproxy.cfg }
  spec:                             # kind-specific block
    states:
      running:
        input:
          name:    { type: string, required: true }
          enabled: { type: boolean, default: true }
      stopped:
        input:
          name: { type: string, required: true }
  ```

  Common root-level fields: `kind`, `protocol_version`, `namespace`, `name`, `required_capabilities`, `side_effects`. Kind-specific — in `spec:`:
  - `spec.states` (map<state-name, {input}>) — for `kind: soul_module`.
  - `spec.profile_schema` (JSON Schema) — for `kind: cloud_driver` (the VM-profile schema, see [`docs/keeper/cloud.md`](../keeper/cloud.md)).
  - `spec.provider_kind` (enum / string) — for `kind: ssh_provider` (Vault SSH CA / static / Teleport / ...).

  In `sdk/handshake/` — a single Go type `Manifest` with `oneof` sub-messages `SoulModuleSpec` / `CloudDriverSpec` / `SshProviderSpec` (proto-style). Evolving new kinds (`secrets_provider` etc.) — adding a variant to the enum without breaking changes.

  The binary name (`soul-mod-*` / `soul-cloud-*` / `soul-ssh-*`) is a **convention, not a contract**. A cross-check `manifest.kind == "soul_module"` && the binary name is `soul-mod-*` → warns in the log on mismatch, does not fail (aliases/symlinks are acceptable).

  Rejected: three separate formats (drift of common fields as they evolve); a hybrid (equivalent to (e)); the binary name as a discriminator (a weak discriminator).

  > **(f) is CORRECTED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377).** `required_capabilities` is **disclosure to the operator before approval, not a control**. There is no sandbox at plugin spawn — no `SysProcAttr`, no seccomp, no `Setuid`, no rlimit anywhere in `shared/pluginhost` — so a plugin that declares `[]` and then opens a socket is stopped by nothing. Two further statements in the paragraph below are false as written: `soul-lint` does **not** check capabilities at all (it reads `required_capabilities` only to validate the enum), and the check that does exist runs in the host at spawn (`pluginhost.CheckCapabilities`), which compares strings and refuses to exec. The enum itself and the `allowed_capabilities` spawn refusal are kept — see the amendment for what each is worth.

  **(f) `required_capabilities` — an enum with a fixed starting set.** A plugin declares what it needs from the host system. `soul-lint` statically checks: the plugin's `required_capabilities` ⊆ the host's `plugin_runtime.allowed_capabilities` ([`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) / [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)). A mismatch → a destiny validation error **before launch**.

  Starting set (closed enum, MVP):
  | Capability | Meaning |
  |---|---|
  | `run_as_root` | The host process (`soul` / `keeper`) must have UID 0 when launching the plugin. |
  | `network_outbound` | The plugin makes outbound network calls (cloud API, vault, package mirror). |
  | `network_inbound` | The plugin listens on a port (a rare case, for test helper plugins). |
  | `vault_access` | The plugin talks to Vault via the client helper SDK. |
  | `fs_write_root` | The plugin writes outside `/var/lib/soul-stack/`. |
  | `exec_subprocess` | The plugin runs external commands via `os/exec`. |

  Extending the list is done via a PR to `proto/plugin/vN/manifest.proto`, not breaking. Freeform extensions with an `x-` prefix (open-ended capabilities) are **rejected in the MVP** — will be added on the first real request.

  > **(g) is CORRECTED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377).** `side_effects` is **disclosure to the operator before approval, not a contract**. All three host behaviours listed below — audit trail, conflict detection, runtime violation — **do not exist**. `SideEffects` is read in exactly one place in the whole tree, the grammar validator at `shared/plugin/manifest.go:477`; nothing consumes the values. No production code emits `policy_violation` — the string appears only in test fixtures. `plugin_runtime.conflict_policy` is parsed and enum-validated in `shared/config` and read by nothing. The declaration and its grammar are kept — see the amendment for why.

  **(g) `side_effects` — a strict contract.** A plugin must list **all resources** it touches (touched resources). Grammar: a list of entries of the form `{<resource-type>: <value>}`, where `<resource-type>` is a closed enum:

  | Resource type | Meaning |
  |---|---|
  | `service` | a service name (`haproxy`, `redis-server`). |
  | `file` | an absolute path to a file (`/etc/haproxy/haproxy.cfg`). |
  | `package` | an OS package name (`haproxy`, `nginx`). |
  | `port` | a tcp/udp port as an int (`80`, `443`). |
  | `user` | an OS username (`postgres`). |
  | `group` | an OS group name. |
  | `directory` | an absolute path to a directory. |
  | `cron` | a cron-job name. |
  | `mount` | a mountpoint (`/var/lib/data`). |

  Extending the enum is done via a PR to `proto/plugin/vN/`, not breaking. Host behavior:
  - **Audit trail:** each touched resource is written to an audit event naming the plugin and the `apply_id`.
  - **Conflict detection:** two plugins in the same run claiming the same resource → a warning or a fail (the resolution policy is `plugin_runtime.conflict_policy`, normalized in [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) and [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)).
  - **Runtime violation:** a plugin touches a resource not declared in `side_effects` → the step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` (the exact field form is a separate task, see backlog), and the event `task.policy_violation` is written to the shared audit pipeline ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — normalizes storage / schema / write-path for all audit events, including `side_effects` violations). Introducing a new field or a new status in [ADR-012](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) is **not fixed here** — that is a change to the proto contract, a separate propose-and-wait when `proto/plugin/v1/` is finalized.

  Wildcard values (`file: /etc/haproxy/**`) and conditional `side_effects` (`when: …`) are **rejected in the MVP** — will be added on the first real request.

  **(h) TLS on the plugin socket — not in the MVP.** Security via file permissions: the Unix domain socket sits in a host-managed directory, mode `0700`, owned by the service user. Other processes physically cannot open the socket.

  HashiCorp uses mTLS over TCP-loopback — there it is justified (any process on the host can connect to a loopback port). We use a Unix socket — that threat does not exist for us. The cost of mTLS on every one-shot plugin launch: +50–150 ms for the TLS handshake, with no benefit over file permissions.

  Extension to mTLS post-MVP — **without breaking changes**: the `server_cert` field (base64-PEM) is already reserved in the JSON handshake; enabled via `plugin_runtime.enable_tls: true` ([`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) / [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime)).

- **Consequences.**
  - **`proto/plugin/v1/*.proto`** is set up with five files: `handshake.proto` (the JSON string format expressed proto-style, for generating the Go struct in `sdk/handshake/`), `manifest.proto` (a typed `Manifest` with `oneof spec`), and three service files — `soulmodule.proto`, `clouddriver.proto`, `sshprovider.proto`. **Not finalized in this ADR** — a separate task after ADR-020.
  - **`sdk/handshake/`** — a single Go helper for all three kinds: reads the env var `SOUL_PLUGIN_SOCKET`, writes the JSON handshake to stdout, opens the Unix socket, registers the gRPC server, handles SIGTERM.
  - **`soul-lint`** must understand the manifest for static destiny validation: unknown module, unknown `state`, wrong parameters (the `input` schema), `required_capabilities` ⊄ the host's `allowed_capabilities`, unknown `kind`.
  - A `plugin_runtime:` block appears in **`keeper.yml`** / **`soul.yml`** (`socket_dir`, `startup_timeout`, `shutdown_grace`, `allowed_capabilities`, `conflict_policy`, opt. `enable_tls`) — normalized in [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) and [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime), with a per-field hot-reload policy.
  - **`docs/keeper/plugins.md`** is rewritten normatively: manifest field tables, the handshake JSON schema, a lifecycle diagram, capabilities and side_effects enum tables, complete manifest examples for all three kinds.
  - **`docs/soul/modules.md`** gains a brief section on the SoulModule manifest, cross-linking to `docs/keeper/plugins.md` as the normative source.
  - **`docs/naming-rules.md`** gains the names `kind`, `Manifest`, `Handshake`, the capabilities enum, the resource-types enum, `plugin_runtime`.
  - Closes two open sub-Q's in [§"Module model"](../architecture.md#module-model): "a separate `manifest.yaml` vs an RPC `Manifest()`" (in [§"Module schema document"](../architecture.md#module-schema-document)) — ⚠ **both answers are superseded 2026-08-06: the schema is neither authored nor served over RPC, it is generated and stamped** — and "file name and exact protocol version" (the file-name half is now moot, NIM-377) (in [§"Module protocol — gRPC over stdio (HashiCorp-style)"](../architecture.md#module-protocol---grpc-over-stdio-hashicorp-style)).
  - `examples/` are updated after `proto/plugin/v1/*.proto` is finalized (not in this ADR).

- **Trade-offs.**
  - **⚠ Manifest ↔ plugin-code drift — SUPERSEDED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377).** The risk below is **removed at the root**, not mitigated better: there is one declaration (a `module.Def` in Go) and the schema is derived from it, so the two cannot disagree ((n)). The tool that shipped is **`soul-mod verify`** — a CI gate that an artifact's *stamped* schema still matches its code — not the `soul-mod gen-manifest --check` proposed below, and the self-test is not needed. Original text:
  - **Manifest ↔ plugin-code drift.** A real risk: a plugin author forgets to update `manifest.yaml` after changing `Apply`. Mitigation — a self-test (running with invalid input → `INVALID_ARGUMENT`) and a post-MVP `soul-mod gen-manifest --check` from the SDK. The alternative "RPC-only Manifest()" removes the drift but breaks `soul-lint`'s offline validation — a more expensive price.
  - **One-shot lifecycle vs cold-start cost.** Every Apply spins up the plugin process from scratch; for long-running scenarios with dozens of calls to the same plugin this is 50–200 ms × N overhead. Acceptable for the MVP; long-lived — a separate ADR once the first performance profile hits this wall.
  - **`server_cert` in the handshake JSON is always empty in the MVP.** Cruft (one unused field), but it provides forward-compat for future mTLS without changing the proto/JSON format.
  - **Closed enums for capabilities / side_effects.** Any new capability or resource-type requires a PR to the proto contract. This is a deliberate price for `soul-lint`'s static validation (open-ended `x-` keys would make the validation meaningless). Extending the enum is minor per [ADR-007](0007-versioning-git-ref.md#adr-007-artifact-versioning--via-git-ref-not-a-manifest-field) (a Go-library tag), not breaking.
  - **File permissions instead of mTLS.** On multi-tenant hosts with unprivileged processes from other users, file perms are equivalent to mTLS (nobody can open a 0700 socket owned by another user). Under a root compromise, both options lose equally. This matches Soul Stack's threat model — correctly.

- **Amendment (2026-05-26, SshProvider — the MVP set is closed).** Following the `keeper.push` pilots, the final set of SSH providers is fixed along with decisions on three shared mechanics (credentials-flow, key-ownership, params-delivery). Decisions made by the user on 2026-05-26.
  - **(i) The `SshProvider` MVP set — three plugins, committed and working.** `soul-ssh-static` (commit `4f95ef6`) — the reference; `soul-ssh-vault` (commit `3642520`, dispatcher S2 ephemeral keypair); `soul-ssh-teleport` (commit `af27678`, only-add in proto: `SignReply.proxy_jump` field 4). Binaries — `soul-ssh-{static,vault,teleport}`, `kind: ssh_provider` names in the manifest, field `spec.provider_kind ∈ {static_key, vault_ssh_ca, teleport}` (a closed enum for `kind: ssh_provider`, symmetric to [ADR-026(c)](0026-sigil.md#adr-026-sigil--plugin-integrity-keeper-signed-digest-index) for cloud drivers). Extending the enum — propose-and-wait + a PR to [keeper/plugins.md → Schema document](../keeper/plugins.md#schema-document) and [naming-rules.md](../naming-rules.md).
  - **(j) Credentials-flow for Vault SSH CA — Variant B (the plugin itself talks to Vault via the `vault_access` capability).** This diverges from cloud-Variant A ([ADR-017 amendment (d)](0017-keeper-side-core.md#adr-017-keeper-side-core-modules-extended-corecloudprovisioned-corevaultkv-read)) **deliberately**: cloud creds are a **static KV secret** (Keeper resolves the KV → the plugin receives plaintext), for which Variant A is correct. `ssh/sign` is a **Vault operation** (minting a certificate from the operator's pubkey), not reading a value; Variant A for an operation would mean Keeper becomes a Vault-SSH proxy duplicating the Vault SSH engine's logic. Variant B leaves the operation where it is native. The `vault_access` capability stays in the `soul-ssh-vault` manifest (unlike cloud plugins, where it is removed).
  - **(k) Key-ownership for Vault SSH CA / Teleport — Keeper-ephemeral (the private key never leaves Keeper).** Keeper-side (the `keeper.push` dispatcher) generates an ephemeral SSH keypair per session, sends **only the public key** in `SignRequest.public_key`. The plugin signs the pubkey via the Vault SSH CA / Teleport CA and returns only `certificate` (+ opt. `proxy_jump` for Teleport, see (i)); the `private_key` field in `SignReply` is **always empty** for CA providers (only filled by `static_key`, which is itself key material). Rationale — security-first (CLAUDE.md): the fewer points the private key passes through, the smaller the leak surface; the provider never sees the user's private key at all.
  - **(l) Params-delivery — a per-plugin env convention.** The host passes provider parameters (Vault mount-path, Teleport-proxy URL, …) to the plugin via env variables with fixed names:
    - `SOUL_SSH_STATIC_PARAMS` — for `soul-ssh-static` (JSON, the provider's own form).
    - `SOUL_SSH_VAULT_PARAMS` — for `soul-ssh-vault` (JSON, `{mount, role, ttl, ...}`).
    - `SOUL_SSH_TELEPORT_PARAMS` — for `soul-ssh-teleport` (JSON, `{proxy_addr, role, ...}`).

    A generic mechanism (a handshake `PluginParams` field in the JSON handshake) is **deferred post-MVP** — the pilots did not show a need for it (parameter shapes diverge between providers, a typical JSON blob in an env var is simpler than building out a shared schema validator). Once a fourth provider with overlapping parameters appears — revisit through propose-and-wait.
  - **(m) Open item (S3 dispatcher `proxy_jump` support).** The Teleport pilot returns the bastion address to route the SSH session through in `SignReply.proxy_jump`, but the dispatcher (`keeper/internal/push`) **IGNORES** the field — `net.Dial(host:port)` goes **directly**. A full Teleport-via-bastion flow requires dispatcher proxy_jump support (a separate slice worked on in parallel with this canon fixation). Until then the pilot only applies to **hosts with direct SSH reachability**; Teleport-via-bastion will become functional after the dispatcher slice. This is **not an SshProvider problem** — the plugin correctly returns the field, the only-add contract is finalized; the open question is in `keeper.push`'s host-side flow.

> **⚠ Three claims in the ADR-065 amendment below are SUPERSEDED by the [2026-08-06 amendment](#amendment-2026-08-06-nim-377-the-schema-is-generated-from-go-the-artifact-carries-no-name) (NIM-377).** The catalog entry, the resolver reuse, the Sigil flow, `FetchModule` and `core.module.installed` are all unchanged — only these three:
>
> | Claim below | What is true now |
> |---|---|
> | "the manifest on the Soul side is materialized from `PluginSigil.manifest_raw`" | The field is **`PluginSigil.schema`** (number 9), carrying canonical schema-document bytes. Field 6 `manifest` is **reserved, never reused** ([ADR-012](0012-keeper-soul-grpc.md) forbids reuse), as are 1 `namespace` and 2 `name`. |
> | "the Soul-side cache layout is `<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}`" | The slot is **`<paths.modules>/<alias>/`** — named by the registration alias — holding **one executable whose filename means nothing** and the schema document. There is no `<ns>-<name>` and no sibling `manifest.yaml` ([ADR-065 amendment](0065-core-module-installed.md#amendment-2026-08-06-nim-377-the-slot-is-named-by-the-alias-and-the-schema-rides-in-the-artifact)). |
> | "the manifest format (e) is NOT changed" | It **is** changed — that is this ticket. (e) is superseded: generated canonical JSON, no `namespace:`/`name:`, no binary-name convention. |
>
> The line "the manifest ... not from a git checkout" also reads differently now: the schema is stamped **into the artifact**, so it arrives with the bytes as well as in the snapshot.

- **Amendment ([ADR-065](0065-core-module-installed.md), the `plugins.soul_modules[]` catalog + delivery of SoulModule to Soul hosts).** The config catalog `keeper.yml::plugins` is extended with a third kind of entry — **`soul_modules[]`** (`{name, source, ref}`, the same format as `cloud_drivers`/`ssh_providers`): previously, the catalog's git resolution ([ADR-026(g)](0026-sigil.md), `keeper/internal/plugingit`) only served keeper-side kinds. SoulModule entries are resolved by **the same resolver** into the same `cache_root` (reusing the hardening: scheme allowlist / size limits / fail-closed per entry) and go through **the same Sigil flow** (`plugin.allow` → `plugin_sigils`). Delivery of the bytes to the Soul host is a new server-streaming RPC `FetchModule` ([ADR-012 amendment](0012-keeper-soul-grpc.md)) + a Soul-side step `core.module.installed` (an allow-check BEFORE fetch, a full verify by `shared/pluginhost` before an atomic rename, hot-register without restarting the daemon). The lifecycle model (d) (one-shot sub-process) and the manifest format (e) are NOT changed; the manifest on the Soul side is materialized from `PluginSigil.manifest_raw` (delivered via a `SigilSnapshot`), not from a git checkout. The Soul-side cache layout is catalog-style, `<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}` ([soul/modules.md](../soul/modules.md)). Full fixation — [ADR-065](0065-core-module-installed.md).

- **Amendment (2026-07-03, separating Teleport paths: bootstrap delivery bypasses the plugin infrastructure).** Clarifies (i)/(m) after the introduction of the Teleport transport for bootstrap delivery ([ADR-063](0063-bootstrap-token-delivery.md) amendment "Teleport by-name transport", live-proven by the live create run, production profile — [ADR-066](0066-teleport-onboarding-profile.md)). Teleport exists in Soul Stack in **two independent roles**, and they must not be confused:
  - **`core.bootstrap.delivered` `transport: teleport`** — a keeper-side Teleport Dialer on an **identity file** (`keeper.yml::push.teleport`): transport + user-auth + host-verify go entirely through Teleport; **the `soul-ssh-teleport` plugin does NOT participate in this flow**, `Authorize`/`Sign` are not called (the `ssh_provider` name only appears in the audit payload). The `SshProvider` contract (i)–(l) is NOT changed by this.
  - **`soul-ssh-teleport` (the SshProvider plugin)** — remains the signing provider for push runs of Destiny (`SshDispatcher.SendApply`). Limitation (m) — the dispatcher ignores `SignReply.proxy_jump`, "Teleport-via-bastion will become functional after the dispatcher slice" — **remains open only for this path** and does NOT block bootstrap delivery/onboarding (that has its own Dialer). The priority of the dispatcher slice is accordingly lowered: the one known live case of Teleport access is closed via the bootstrap path.

## Amendment (2026-08-06, NIM-377): the schema is generated from Go, the artifact carries no name

Supersedes (a) and (e), corrects (f) and (g), amends (d). Decisions settled with the user 2026-08-06.

**(n) No hand-written manifest — the source of truth is Go.** A module declares itself as a `module.Def` value next to its own code (`sdk/module`); a `module.Bundle` names the set of `Def`s one artifact serves. `manifest.yaml` is not authored, not shipped and not parsed. The drift risk that (a) mitigated with a self-test and an optional `soul-mod gen-manifest --check` is **removed at the root**: there is only one declaration, and the schema is derived from it.

`Def.Input` keeps every field `plugin.InputParamDef` could express — type / required / secret / pattern / description / default, the [ADR-045](0045-param-dsl.md) form fields (`enum`, `format`, `source`, `items`, `multiline`, `example`) and the `introduced_in` / `deprecated` metadata of [ADR-0076](0076-engine-compat-window.md). Nothing the manifest could say becomes inexpressible.

**(o) The schema document — generated, canonical JSON, published twice.** One generator emits it and both copies come from that generator:
- **stamped into the artifact** by `soul-mod stamp`, so Keeper always has it;
- **written to `dist/schema.json`**, so `soul-lint` need not download the binary.

`soul-mod verify` is the CI gate: an artifact whose stamped schema disagrees with its code fails the build. Keeper reads the schema at `plugin.allow` **without executing the artifact** — at that moment the binary is not yet approved, and executing it would defeat the approval it is asking for. This is the same offline-validation requirement (a) was written to satisfy ([ADR-009](0009-scenario-dsl.md#adr-009-scenario--the-full-destiny-task-dsl-the-boundary-with-destiny-is-a-recommendation)), met without a second authored file.

**Format — canonical JSON**, deterministic bytes: sorted keys, no insignificant whitespace. Authors never read or write the document, so goccy's line/column diagnostics stop earning their cost, while determinism starts mattering: the bytes are hashed and signed ([ADR-026](0026-sigil.md)). `NormalizeManifestBytes` collapses to identity for this form.

**Stamping mechanism — a trailer:** the payload plus a fixed-size footer (its length and a versioned magic marker) appended after the executable image. No ELF/Mach-O/PE parsing, readable by seeking from the end, and loaders ignore trailing bytes. **Readers fail closed** on a missing or malformed trailer — never fall back to a neighbouring file, never treat "no schema" as "empty schema".

**(p) The artifact carries no subject name.** No `namespace:`, no `name:`, no publisher, no self-identity of any kind. An artifact knows only its own modules (`acl`, `config`, `info`). Address level 1 comes entirely from registration: the alias `redis` gives `redis.acl.present`; the **same** artifact registered as `redis-community` gives `redis-community.acl.present`, with no change to the artifact and no rebuild. Two publishers of the same subject cannot collide, because the operator picks both names.

Consequently **`Manifest.BinaryName()` goes away** — with no self-name there is nothing to compute. `dist/` holds exactly one executable and Keeper takes it; the host slot is named by the registration alias, not by `<ns>-<name>`. NIM-423 (twelve plugins building a filename the loader does not look for) is closed as **moot**: the filename stops meaning anything. `kind:` stays — it is a type discriminator, not a name, and the soul-host still accepts only `soul_module` while the keeper-host accepts only `cloud_driver` / `ssh_provider`.

The full addressing model is **NIM-376**, a separate open ticket; this amendment fixes only where level 1 comes from.

**(q) Dispatch is a subcommand.** One artifact serves several modules and the host names the one it wants as the first argument: `soul-mod-redis acl`. `ServeBundle` dispatches on it and also answers a `schema` subcommand. **No proto change, `protocol_version` does not move** — the host already forks per Apply ((d), one-shot), so serving several modules from one process would buy nothing.

**(r) `required_capabilities` and `side_effects` are disclosure, not controls.** This is the correction to (f) and (g), and it is a correction of fact, not a change of policy — the code never did what those paragraphs promised:

| Claim in (f) / (g) | What the code does |
|---|---|
| A plugin touching a resource outside `side_effects` fails with `policy_violation` | No production code emits `policy_violation`; the string exists only in test fixtures. `SideEffects` is read in exactly one place — the grammar validator at `shared/plugin/manifest.go:477`. Nothing consumes the values. |
| Conflict detection between two plugins claiming one resource, resolved by `plugin_runtime.conflict_policy` | `conflict_policy` is parsed and enum-validated in `shared/config` and read by nothing. No conflict is ever detected. |
| Each touched resource is written to an audit event | No such event is written. |
| `required_capabilities` is a control on what the plugin may do | There is **no sandbox at spawn** — no `SysProcAttr`, no seccomp, no `Setuid`, no rlimit anywhere in `shared/pluginhost`. `CheckCapabilities` compares strings and refuses to exec; a plugin declaring `[]` and then opening a socket is stopped by nothing. |
| `soul-lint` statically checks `required_capabilities` ⊆ `allowed_capabilities` | `soul-lint` does not read capabilities at all. The only check is the host's at spawn, and with `allowed_capabilities` unset (the default) it is a no-op. |

**The fields stay, and their purpose is real: an operator reads them before approving a binary.** `plugin.allow` is a human act — "this artifact, this digest, may run with the service user's privileges" — and what the artifact says it will touch is exactly the material that act needs. Deleting the declarations would not make the system safer; it would remove the only thing the operator has to read. What changes is the claim: they inform a decision, they do not enforce one.

**The one actual control is unchanged.** The operator approves a specific sha256 and the host refuses to exec anything whose digest differs ([ADR-026](0026-sigil.md)). That gate needs no manifest, does not depend on any declaration being honest, and is not weakened by anything above.

Turning either field into a control is a **separate ADR** with its own cost: enforcing `side_effects` means interposing on the plugin's syscalls, and enforcing `required_capabilities` means an actual sandbox. Neither is designed here, and neither should be assumed to exist because a manifest key spells it.

**(s) Reserved namespace names** — one closed list, checked both at registration and in destiny `required_modules:`. **Mandatory:** `core`, `keeper`, `soul`. **Second tier:** `destiny`, `scenario`, `service`, `incarnation`, `soulprint`, `coven`, `archon`, `sigil`, `soulstack`, `soul-stack`, `local`, `default`, `internal`, `test`, `example`. Registering an alias from this list is refused. This absorbs **NIM-375**. The list lives in [naming-rules.md → Reserved namespace names](../naming-rules.md#reserved-namespace-names).

**Consequences.**
- **`sdk/module`** gains `Def` / `State` / `Input` / `Param` / `Capability` / `SideEffect` / `Bundle` / `Compat`, `ServeBundle` with subcommand dispatch and a `schema` subcommand, and the canonical serializer. `shared/plugin` parses the schema document in place of `manifest.yaml`.
- **`soul-mod`** gains `stamp` and `verify`.
- **`shared/pluginhost`**: `Manifest.BinaryName()` and its discovery use are deleted; a slot holds one executable, named by the alias.
- **No backcompat with the authored-manifest form.** There is no release yet — the old path is deleted, not branched on ([CLAUDE.md](../../CLAUDE.md)).
- **Guard tests, e2e > integration > unit:** a stamped artifact whose schema disagrees with its code must fail `verify`; a slot whose trailer is missing or corrupt must fail closed, not fall back.
- **Not settled by this amendment:** the authoring form for `cloud_driver` / `ssh_provider` / `soul_beacon`. The mechanism above — source-keyed registry, alias-named slot, one executable in `dist/`, no self-name — is forced on every kind, because discovery and the slot layout are shared code. The Go-side generator, however, is specified only for SoulModule bundles (`sdk/module`); what replaces the authored `spec.profile_schema` / `spec.provider_kind` / `spec.params_schema` for the other three kinds is open, and is not decided here.
