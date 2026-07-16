# ADR-020. Plugin Infrastructure: manifest format, handshake, lifecycle

- **Context.** Soul Stack has three categories of plugins with different service contracts (`SoulModule`, `CloudDriver`, `SshProvider`), but a **single infrastructure** — handshake, launch method, manifest format, versioning (see the ["Plugin infrastructure"](../architecture.md#plugin-инфраструктура) section, [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). At the time of this fixation, this infrastructure was only described in sketch form:
  - [§"Module manifest"](../architecture.md#манифест-модуля) is given as a draft example, with an explicit open sub-Q of "a separate `manifest.yaml` next to the binary vs the first RPC method `Manifest()`".
  - [§"Module protocol — gRPC over stdio (HashiCorp-style)"](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style) — a general reference to the HashiCorp `go-plugin` model, with an open sub-Q of "file name and exact protocol version".
  - The handshake-string format, the list of `required_capabilities`, the `side_effects` grammar, plugin lifecycle, the socket path, and the TLS policy are nowhere normatively fixed.

  Without a normative fixation, we can neither finalize `proto/plugin/v1/*.proto` (the next task after this ADR), nor write a unified handshake helper in `sdk/handshake/`, nor implement static destiny validation in `soul-lint` (the latter being a requirement of [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация)).

- **Decision.**

  **(a) Manifest — a static `manifest.yaml` in the plugin's repo.** The file sits at the root of the plugin's repository and ships alongside the binary (in Keeper's cache / on the Soul host). `soul-lint` parses it **without running the binary** — this is a direct requirement of [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (static validation of destiny without bringing up the plugin process).

  The alternative "RPC-only via a `Manifest()` method" is rejected: it breaks `soul-lint`'s offline validation (the plugin would need to be downloaded and started first). The "hybrid" alternative is rejected as over-engineering with no benefit.

  Drift "the manifest is out of sync with the plugin's actual code" is a real risk; it is mitigated via a plugin self-test (calling `Apply` with parameters outside the input schema must return `INVALID_ARGUMENT`) and an optional generated manifest (`soul-mod gen-manifest --check`) — an SDK extension post-MVP, without breaking changes.

  **(b) Handshake — a single-line JSON with a magic prefix field.** On startup the plugin writes **exactly one line** to stdout:

  ```json
  {"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/wb-haproxy-12345.sock","server_cert":""}
  ```

  - The magic field `"soul_stack":"plugin-v1"` is a sanity check. The host ignores all stdout lines before the first line with this field (protection against an accidental `fmt.Println` in the plugin's `init()` or output from libraries).
  - Fields: `soul_stack` (a constant string, `"plugin-v1"`), `protocol_version` (int), `kind` (an enum, see (e)), `network` (an enum `unix`, extended to `named_pipe` / `tcp` post-MVP), `address` (the socket path), `server_cert` (base64-PEM or an empty string; reserved for optional post-MVP mTLS, see (h)).
  - Extension via new **optional** keys (`features`, `capabilities`, …) — without breaking changes.
  - **We do not use the `hashicorp/go-plugin` library as a dependency.** Its 6-field pipe-string format is excessive and inflexible; the MPL-2.0 copyleft is not needed (see [ADR-016](0016-parity-license.md#adr-016-стратегия-parity-с-saltstackansible-и-лицензия-soul-stack)). Only the "one-line handshake → gRPC-over-socket" model is borrowed.

  Rejected: (a) the HashiCorp 6-field pipe string (excessive fields + a rigid format), (b) a minimal pipe (extension = breaking), (c) framed exchange (unreadable, doesn't fit the one-line-handshake model).

  **(c) Versioning — `protocol_version` is duplicated in the manifest and in the handshake.** One int, two places:
  - In `manifest.yaml` — for static `soul-lint` (without running the plugin).
  - In the JSON handshake string — for a runtime sanity check **before** opening the gRPC channel.

  The host (`keeper` / `soul` / `soul-lint`) holds the constant `SupportedProtocolVersions = [1]`. Three cross-checks:
  - `handshake.protocol_version != manifest.protocol_version` → the plugin is invalid, refuse to start (drift inside the plugin).
  - `protocol_version` outside `SupportedProtocolVersions` → a hard fail with the message `protocol_version=N, host supports [...]`.
  - `manifest.kind != handshake.kind` → refuse to start (drift).

  A strict correspondence between **`protocol_version: N` ↔ `proto/plugin/vN/`** (one go.mod submodule per [ADR-011](0011-go-layout.md#adr-011-раскладка-go-кода-gowork-с-модулями-по-сторонам)). Evolution: adding `proto/plugin/v2/` → a host of version N+1 supports `[1, 2]` (forward-compat only-add, the plugin-protocol analog of [ADR-012(g)](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add); never delete/reuse field numbers in `proto/plugin/v1/`).

  `protocol_version` is a **compat API flag, not an artifact version**. This is the exception for `protocol_version` already articulated in [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте); ADR-020 does not introduce a new exception, it only fixes its place within the plugin infrastructure.

  Rejected: (a) handshake-only / (d) gRPC reflection — both break [ADR-009](0009-scenario-dsl.md#adr-009-scenario--полная-dsl-задач-destiny-граница-с-destiny--рекомендация) (offline validation); (b) manifest-only — drift is not caught.

  **(d) Socket + lifecycle.**
  - **Socket type:** Unix domain socket only in the MVP. The `network` field in the handshake JSON allows extending the enum (`unix | named_pipe | tcp`) — Windows support post-MVP without breaking changes.
  - **Socket path:** the host passes it via the env var **`SOUL_PLUGIN_SOCKET`**. The directory is `/var/run/soul-stack/plugins/<namespace>-<name>-<pid>.sock` for a Soul host, `/var/run/soul-stack-keeper/plugins/<namespace>-<name>-<pid>.sock` for a Keeper host; mode `0700`, owned by the service user (`soul` or `keeper`). The SDK trivially reads the env var and opens the socket.
  - **Startup:** the host forks the plugin process, passes `SOUL_PLUGIN_SOCKET`, reads stdout until the first line with `"soul_stack":"plugin-v1"`. All lines before that are ignored (but logged at debug level).
  - **Shutdown:** the host sends SIGTERM; the SDK provides a signal-handler helper, the plugin finishes its current RPCs and exits. A `Shutdown()` RPC is not introduced into the MVP proto contract (an extension in `proto/plugin/v2/` if needed). Grace period 10s — if the plugin has not exited — SIGKILL.
  - **Lifecycle:** **one-shot** — started for each Apply, exits afterward. Long-lived (one process for a series of calls) — a separate ADR if needed (profiling shows a cold-start cost, or a CloudDriver with batched operations against a single cloud-API token appears).
  - **Timeouts (defaults):** startup `10s` (the handshake string must appear), shutdown grace `10s` (SIGTERM → SIGKILL). Specific values are configured via the `keeper.yml` / `soul.yml` block `plugin_runtime:` — the normative specification is in [`docs/keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime) and [`docs/soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime).

  **(e) Manifest format — a single schema with a `kind:` discriminator.** The same YAML format for all three plugin types; differences live in the `spec:` section:

  ```yaml
  # soul-mod-haproxy/manifest.yaml
  kind: soul_module                 # discriminator: soul_module | cloud_driver | ssh_provider
  protocol_version: 1
  namespace: wb
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
  - **Runtime violation:** a plugin touches a resource not declared in `side_effects` → the step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` (the exact field form is a separate task, see backlog), and the event `task.policy_violation` is written to the shared audit pipeline ([ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — normalizes storage / schema / write-path for all audit events, including `side_effects` violations). Introducing a new field or a new status in [ADR-012](0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add) is **not fixed here** — that is a change to the proto contract, a separate propose-and-wait when `proto/plugin/v1/` is finalized.

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
  - Closes two open sub-Q's in [§"Module model"](../architecture.md#модель-модулей): "a separate `manifest.yaml` vs an RPC `Manifest()`" (in [§"Module manifest"](../architecture.md#манифест-модуля)) and "file name and exact protocol version" (in [§"Module protocol — gRPC over stdio (HashiCorp-style)"](../architecture.md#протокол-модулей--grpc-over-stdio-hashicorp-style)).
  - `examples/` are updated after `proto/plugin/v1/*.proto` is finalized (not in this ADR).

- **Trade-offs.**
  - **Manifest ↔ plugin-code drift.** A real risk: a plugin author forgets to update `manifest.yaml` after changing `Apply`. Mitigation — a self-test (running with invalid input → `INVALID_ARGUMENT`) and a post-MVP `soul-mod gen-manifest --check` from the SDK. The alternative "RPC-only Manifest()" removes the drift but breaks `soul-lint`'s offline validation — a more expensive price.
  - **One-shot lifecycle vs cold-start cost.** Every Apply spins up the plugin process from scratch; for long-running scenarios with dozens of calls to the same plugin this is 50–200 ms × N overhead. Acceptable for the MVP; long-lived — a separate ADR once the first performance profile hits this wall.
  - **`server_cert` in the handshake JSON is always empty in the MVP.** Cruft (one unused field), but it provides forward-compat for future mTLS without changing the proto/JSON format.
  - **Closed enums for capabilities / side_effects.** Any new capability or resource-type requires a PR to the proto contract. This is a deliberate price for `soul-lint`'s static validation (open-ended `x-` keys would make the validation meaningless). Extending the enum is minor per [ADR-007](0007-versioning-git-ref.md#adr-007-версионирование-артефактов--через-git-ref-а-не-через-поле-в-манифесте) (a Go-library tag), not breaking.
  - **File permissions instead of mTLS.** On multi-tenant hosts with unprivileged processes from other users, file perms are equivalent to mTLS (nobody can open a 0700 socket owned by another user). Under a root compromise, both options lose equally. This matches Soul Stack's threat model — correctly.

- **Amendment (2026-05-26, SshProvider — the MVP set is closed).** Following the `keeper.push` pilots, the final set of SSH providers is fixed along with decisions on three shared mechanics (credentials-flow, key-ownership, params-delivery). Decisions made by the user on 2026-05-26.
  - **(i) The `SshProvider` MVP set — three plugins, committed and working.** `soul-ssh-static` (commit `4f95ef6`) — the reference; `soul-ssh-vault` (commit `3642520`, dispatcher S2 ephemeral keypair); `soul-ssh-teleport` (commit `af27678`, only-add in proto: `SignReply.proxy_jump` field 4). Binaries — `soul-ssh-{static,vault,teleport}`, `kind: ssh_provider` names in the manifest, field `spec.provider_kind ∈ {static_key, vault_ssh_ca, teleport}` (a closed enum for `kind: ssh_provider`, symmetric to [ADR-026(c)](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс) for cloud drivers). Extending the enum — propose-and-wait + a PR to [keeper/plugins.md → Manifest](../keeper/plugins.md#manifest) and [naming-rules.md](../naming-rules.md).
  - **(j) Credentials-flow for Vault SSH CA — Variant B (the plugin itself talks to Vault via the `vault_access` capability).** This diverges from cloud-Variant A ([ADR-017 amendment (d)](0017-keeper-side-core.md#adr-017-keeper-side-core-модули-расширены-corecloudprovisioned-corevaultkv-read)) **deliberately**: cloud creds are a **static KV secret** (Keeper resolves the KV → the plugin receives plaintext), for which Variant A is correct. `ssh/sign` is a **Vault operation** (minting a certificate from the operator's pubkey), not reading a value; Variant A for an operation would mean Keeper becomes a Vault-SSH proxy duplicating the Vault SSH engine's logic. Variant B leaves the operation where it is native. The `vault_access` capability stays in the `soul-ssh-vault` manifest (unlike cloud plugins, where it is removed).
  - **(k) Key-ownership for Vault SSH CA / Teleport — Keeper-ephemeral (the private key never leaves Keeper).** Keeper-side (the `keeper.push` dispatcher) generates an ephemeral SSH keypair per session, sends **only the public key** in `SignRequest.public_key`. The plugin signs the pubkey via the Vault SSH CA / Teleport CA and returns only `certificate` (+ opt. `proxy_jump` for Teleport, see (i)); the `private_key` field in `SignReply` is **always empty** for CA providers (only filled by `static_key`, which is itself key material). Rationale — security-first (CLAUDE.md): the fewer points the private key passes through, the smaller the leak surface; the provider never sees the user's private key at all.
  - **(l) Params-delivery — a per-plugin env convention.** The host passes provider parameters (Vault mount-path, Teleport-proxy URL, …) to the plugin via env variables with fixed names:
    - `SOUL_SSH_STATIC_PARAMS` — for `soul-ssh-static` (JSON, the provider's own form).
    - `SOUL_SSH_VAULT_PARAMS` — for `soul-ssh-vault` (JSON, `{mount, role, ttl, ...}`).
    - `SOUL_SSH_TELEPORT_PARAMS` — for `soul-ssh-teleport` (JSON, `{proxy_addr, role, ...}`).

    A generic mechanism (a handshake `PluginParams` field in the JSON handshake) is **deferred post-MVP** — the pilots did not show a need for it (parameter shapes diverge between providers, a typical JSON blob in an env var is simpler than building out a shared schema validator). Once a fourth provider with overlapping parameters appears — revisit through propose-and-wait.
  - **(m) Open item (S3 dispatcher `proxy_jump` support).** The Teleport pilot returns the bastion address to route the SSH session through in `SignReply.proxy_jump`, but the dispatcher (`keeper/internal/push`) **IGNORES** the field — `net.Dial(host:port)` goes **directly**. A full Teleport-via-bastion flow requires dispatcher proxy_jump support (a separate slice worked on in parallel with this canon fixation). Until then the pilot only applies to **hosts with direct SSH reachability**; Teleport-via-bastion will become functional after the dispatcher slice. This is **not an SshProvider problem** — the plugin correctly returns the field, the only-add contract is finalized; the open question is in `keeper.push`'s host-side flow.

- **Amendment ([ADR-065](0065-core-module-installed.md), the `plugins.soul_modules[]` catalog + delivery of SoulModule to Soul hosts).** The config catalog `keeper.yml::plugins` is extended with a third kind of entry — **`soul_modules[]`** (`{name, source, ref}`, the same format as `cloud_drivers`/`ssh_providers`): previously, the catalog's git resolution ([ADR-026(g)](0026-sigil.md), `keeper/internal/plugingit`) only served keeper-side kinds. SoulModule entries are resolved by **the same resolver** into the same `cache_root` (reusing the hardening: scheme allowlist / size limits / fail-closed per entry) and go through **the same Sigil flow** (`plugin.allow` → `plugin_sigils`). Delivery of the bytes to the Soul host is a new server-streaming RPC `FetchModule` ([ADR-012 amendment](0012-keeper-soul-grpc.md)) + a Soul-side step `core.module.installed` (an allow-check BEFORE fetch, a full verify by `shared/pluginhost` before an atomic rename, hot-register without restarting the daemon). The lifecycle model (d) (one-shot sub-process) and the manifest format (e) are NOT changed; the manifest on the Soul side is materialized from `PluginSigil.manifest_raw` (delivered via a `SigilSnapshot`), not from a git checkout. The Soul-side cache layout is catalog-style, `<paths.modules>/<ns>-<name>/{manifest.yaml, soul-mod-<name>}` ([soul/modules.md](../soul/modules.md)). Full fixation — [ADR-065](0065-core-module-installed.md).

- **Amendment (2026-07-03, separating Teleport paths: bootstrap delivery bypasses the plugin infrastructure).** Clarifies (i)/(m) after the introduction of the Teleport transport for bootstrap delivery ([ADR-063](0063-bootstrap-token-delivery.md) amendment "Teleport by-name transport", live-proven by run-23, production profile — [ADR-066](0066-teleport-onboarding-profile.md)). Teleport exists in Soul Stack in **two independent roles**, and they must not be confused:
  - **`core.bootstrap.delivered` `transport: teleport`** — a keeper-side Teleport Dialer on an **identity file** (`keeper.yml::push.teleport`): transport + user-auth + host-verify go entirely through Teleport; **the `soul-ssh-teleport` plugin does NOT participate in this flow**, `Authorize`/`Sign` are not called (the `ssh_provider` name only appears in the audit payload). The `SshProvider` contract (i)–(l) is NOT changed by this.
  - **`soul-ssh-teleport` (the SshProvider plugin)** — remains the signing provider for push runs of Destiny (`SshDispatcher.SendApply`). Limitation (m) — the dispatcher ignores `SignReply.proxy_jump`, "Teleport-via-bastion will become functional after the dispatcher slice" — **remains open only for this path** and does NOT block bootstrap delivery/onboarding (that has its own Dialer). The priority of the dispatcher slice is accordingly lowered: the one known live case of Teleport access (WB) is closed via the bootstrap path.
