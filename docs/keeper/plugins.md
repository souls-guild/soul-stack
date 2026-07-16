# Plugin infrastructure Soul Stack

Regulatory specification of the `manifest.yaml` format, handshake strings, plugin lifecycle, versioning, capabilities and side_effects. The source of truth for the solutions is [ADR-020](../adr/0020-plugin-infrastructure.md). This document contains field tables, JSON handshake schema, lifecycle diagram, enum tables, complete manifest examples for all three kinds of plugins.

The document covers **all three kinds of plugins** (manifest format is the same, [ADR-020(e)](../adr/0020-plugin-infrastructure.md)):

| Kind | Host | Binar | Destination |
|---|---|---|---|
| `soul_module` | `soul` (agent or push) | `soul-mod-<name>` | Implements Destiny steps: `SoulModule`. Also see [`../soul/modules.md`](../soul/modules.md). |
| `cloud_driver` | `keeper` (module `keeper.cloud`) | `soul-cloud-<provider>` | Creating/deleting a VM in the cloud: `CloudDriver`. |
| `ssh_provider` | `keeper` (module `keeper.push`) | `soul-ssh-<provider>` | SSH credentials for push run: `SshProvider`. |

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

## Manifest

Static file `manifest.yaml` in **the root of the plugin repository** and **next to the binary** in the host cache ([ADR-020(a)](../adr/0020-plugin-infrastructure.md)). Parsed by `soul-lint` **without running the binary** (requirement [ADR-009](../adr/0009-scenario-dsl.md)).

Operator command: `soul-lint validate-manifest <path> [--json]`. Does: parse YAML, check `kind`/`required_capabilities`/`side_effects`, regex namespace/name/state-name, `protocol_version ∈ SupportedProtocolVersions` closed, kind-specific `spec` (`states` for `soul_module` / `profile_schema` for `cloud_driver` / `provider_kind` for `ssh_provider`) and first-level input-DSL (type/required/secret/pattern). Exit-code: `0` = ok, `1` = there are errors, `2` = I/O fatal. The parser and validator live in `shared/plugin/manifest.go` - a shared source of truth with runtime-discovery in `soul/internal/pluginhost/`.

### General fields (for all kinds)

| Field | Type | Default | Meaning |
|---|---|---|---|
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Plugin type discriminator. private enum; extension - via PR in `proto/plugin/vN/manifest.proto`, without breaking ([ADR-020(e)](../adr/0020-plugin-infrastructure.md)). `soul_beacon` — Soul-side event-driven monitoring plugin (ADR-030 V5-2). |
| `protocol_version` | `int32` | — | Version `proto/plugin/vN/`. Duplicated in the handshake line; cross-check inside the plugin and vs `SupportedProtocolVersions` host ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)). **Not artifact version** is an API compat flag, an exception to [ADR-007](../adr/0007-versioning-git-ref.md). `int32` (and not `int`) is a deliberate exception from the type dictionary: the protocol version will not grow beyond 2³¹, at the wire level the type is fixed in `proto/plugin/v1/manifest.proto`. |
| `namespace` | `string` (kebab-case) | — | Plugin collection. For core: `core`. For third-party: `wb` / `community` / organization name. |
| `name` | `string` (kebab-case) | — | The name of the plugin inside the collection. Addressing is `<namespace>.<name>.<state>` for modules. |
| `required_capabilities` | `list<enum>` | `[]` | Closed enum, see capabilities table. `soul-lint` checks with `plugin_runtime.allowed_capabilities` host. |
| `side_effects` | `list<map<enum,value>>` | `[]` | Strict contract of touched resources, see side_effects table. Runtime violation (the plugin touches a resource not declared in `side_effects`) → the step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` (the exact form of the field is a separate audit-pipeline standardization task for `side_effects`, see backlog). |
| `spec` | kind-specific block | — | Kind-specific fields; the form depends on `kind:` (see below). |
| `binary_sha256` | `string` (hex64) | `""` (optional) | SHA-256 fingerprint of the plugin binary (hex lowercase, exactly 64 characters). Optional - empty until signature **Sigil** ([ADR-026](../adr/0026-sigil.md)); used to verify-against-Sigil before `exec` (see [Integrity-model](#integrity-model)). Type `string` (hex), not `bytes` - consistent with `plugin_sigils.sha256` (TEXT CHECK hex64). |

The input schema format inside `spec:` depends on `kind:`. For `soul_module` - Soul Stack input-DSL ([`docs/input.md`](../input.md)) in `spec.states.<name>.input`. For `cloud_driver` and `ssh_provider` - JSON Schema draft 2020-12 in `spec.profile_schema` / `spec.params_schema` respectively.

### `spec` for `kind: soul_module`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `spec.states` | `map<state-name, {input, description?}>` | — | Map of supported states (or verb forms). The key is the state name (`installed` / `running` / `run` / ...). |
| `spec.states.<name>.input` | input-schema (see [`docs/input.md`](../input.md)) | `{}` | Contract parameters for this state. `soul-lint` validates `params:` of each destiny task against this schema. |
| `spec.states.<name>.description` | `string` (optional) | — | Human-readable description for documentation/UI. |

### `spec` for `kind: cloud_driver`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `spec.profile_schema` | `JSON Schema` | — | VM profile parameters diagram. Used when creating a Profile via OpenAPI/MCP for validation (see [`cloud.md`](cloud.md)). |
| `spec.provider_kind` | `string` (optional) | — | Cloud provider family (`aws` / `gcp` / `yandex-cloud` / `openstack`). Information field. |

### `spec` for `kind: ssh_provider`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `spec.provider_kind` | `string` (convention: `vault_ssh_ca` / `static_key` / `teleport`; extension via PR without proto editing) | — | SSH provider type; affects the UI/documentation, but **not** the contract `Sign`/`Authorize`. At the proto level - an open line for forward-compat (without editing `proto/plugin/vN/`). |
| `spec.params_schema` | `JSON Schema` (optional) | `{}` | Diagram of provider parameters specified in `keeper.yml` (for example, `vault_mount` for `vault_ssh_ca`). |

### `spec` for `kind: soul_beacon`

Soul-side event-driven monitoring plugin ([ADR-030 V5-2](../adr/0030-vigil-oracle.md) + [amendment 2026-05-26](../adr/0030-vigil-oracle.md#amendment-2026-05-26-s5-closure), binary `soul-beacon-<name>`). Read-only by design: `Check` observes the state of the host and does NOT mutate the system. Vigil addressing is `<namespace>.<name>` in the `VigilDef.check` field (the Soul-side dispatcher distinguishes built-in `core.beacon.*` from plugin-beacons by namespace).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `spec.params_schema` | `JSON Schema` (optional) | `{}` | Scheme `params` Vigil, specified by the operator via OpenAPI/MCP. Runtime checks (what is not expressed by JSON Schema) - via `SoulBeacon.Validate`. |

Not allowed: `spec.states` (one operation type - `Check`, no SoulModule state semantics), `spec.provider_kind` (only for `ssh_provider`), `spec.profile_schema` (only for `cloud_driver`).

### manifest extension

New kinds (`secrets_provider`, `audit_sink`, ...) - adding a variant to enum `kind` and a new submessage `*Spec` to `proto/plugin/vN/manifest.proto`. Forward-compat: host of earlier version sees unknown `kind:` → rejects plugin with message `unknown kind=X, host supports [...]`.

### Drift manifest ↔ plugin code

The discrepancy between the declaration in `manifest.yaml` and the actual behavior of the code is a real risk. Protection:

- **Self-test:** the plugin must return `INVALID_ARGUMENT` when calling `Apply` with parameters outside the input-schema. It is assumed that the SDK generates the validator from the schema automatically.
- **Cross-check `kind`:** `manifest.kind != handshake.kind` → host rejects startup ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)).
- **Generated manifest (post-MVP):** `soul-mod gen-manifest --check` from SDK - compares declarations in code vs `manifest.yaml`. Not part of the MVP.

## Handshake

When launched, the plugin writes **exactly one line** with JSON-payload to stdout. All lines up to the first with the magic field `"soul_stack":"plugin-v1"` host are **ignored** (logged at the debug level). After a handshake, stdout is considered closed to the plugin protocol - any subsequent entries to stdout are ignored by the host. Plugin logs in MVP are written to **stderr** (standard UNIX channel for diagnostics); host forwards the plugin's stderr to its log/OTel-pipeline with the tag `plugin=<namespace>.<name>`. Structured log-stream via separate gRPC-RPC - reserved under `proto/plugin/v2/`, not available in MVP.

### Handshake string format

```json
{"soul_stack":"plugin-v1","protocol_version":1,"kind":"soul_module","network":"unix","address":"/var/run/soul-stack/plugins/wb-haproxy-12345.sock","server_cert":""}
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `soul_stack` | `string` (constant `"plugin-v1"`) | — | Magic sanity field. Host ignores all stdout lines up to the first with this field. The value is independent of `protocol_version` - this is the "handshake-string format v1" marker; changes only when breaking changes the handshake format itself (separate ADR). |
| `protocol_version` | `int32` | — | Plugin protocol version (see [Versioning](#versioning)). Must match `manifest.protocol_version`. The type `int32` (not `int`) is the same intentional exception as in the common Manifest fields. |
| `kind` | `enum{soul_module,cloud_driver,ssh_provider,soul_beacon}` | — | Must match `manifest.kind`. |
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
| `manifest.protocol_version != handshake.protocol_version` | Hard fail: drift inside the plugin. SIGTERM. |
| `manifest.kind != handshake.kind` | Hard fail: drift inside the plugin. SIGTERM. |
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
host (keeper / soul)                              plugin (soul-mod-* / soul-cloud-* / soul-ssh-*)
─────────────────────                              ─────────────────────────────────────────────
   1. mkdir /var/run/soul-stack/plugins/  (mode 0700, owned by service user)
   2. socket_path := "/var/run/.../plugins/<namespace>-<name>-<pid>.sock"
   3. fork():
      env SOUL_PLUGIN_SOCKET=<socket_path>
      exec <plugin_binary>             ─────────►   init(); read env SOUL_PLUGIN_SOCKET
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

### Lifecycle parameters (configurable via `plugin_runtime:` block)

Block `plugin_runtime:` in [`keeper.yml`](config.md) / [`soul.yml`](../soul/config.md) - regulatory specification: [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper-side) and [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul-side). Defaults are fixed in [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md); the table below duplicates them inline for ease of reading this document.

| Parameter (in `plugin_runtime:`) | Default | Meaning |
|---|---|---|
| `startup_timeout` | `10s` | Time from fork to the appearance of the handshake line. Excess → SIGTERM. |
| `shutdown_grace` | `10s` | Time from SIGTERM to SIGKILL. |
| `allowed_capabilities` | all 6 capabilities from table | List of capabilities allowed on this host; `soul-lint` checks against `manifest.required_capabilities`. |
| `conflict_policy` | `warn` | What to do if there is a conflict between `side_effects` two plugins: `warn` / `fail`. |
| `enable_tls` | `false` | Post-MVP option: enable mTLS on the plugin socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md)). |

Full field typing, value validation and per-field hot-reload policy - in [`config.md → plugin_runtime`](config.md#plugin_runtime) (Keeper) and [`../soul/config.md → plugin_runtime`](../soul/config.md#plugin_runtime) (Soul).

### Socket location

| Host | Directory | Mode | Owner |
|---|---|---|---|
| `soul` | `/var/run/soul-stack/plugins/` | `0700` | service user `soul` |
| `keeper` | `/var/run/soul-stack-keeper/plugins/` | `0700` | service user `keeper` |

The socket file name is `<namespace>-<name>-<pid>.sock` (the dot from the plugin's `<namespace>.<name>` addressing has been replaced with a hyphen for consistency with file grammar). Example: for plugin `wb.haproxy` with pid `12345` → `wb-haproxy-12345.sock`. After the plugin exits, the host deletes the file (in case the plugin is not unlinked itself).

## Integrity-model

The plugin binary in the host cache is forked with service-user rights (`keeper` plugins have access to Vault / PG / PKI). Substitution of the binary in `/var/lib/soul-stack-keeper/plugins/` (or in artifact-source / with Keeper-checkout git-ref **before** the first appearance on the host) → RCE. Protection - **Sigil** ([ADR-026](../adr/0026-sigil.md)): Keeper-signed digest index (**Option A**). SHA-256 reconciliation (invariant [CLAUDE.md](../../CLAUDE.md) "SHA-256 cache") is saved as defense-in-depth before each `exec`; "trust as is" with first-load **replaced** by Sigil verification.

> **Sigil replaces the previous TOFU model** (trust-on-first-use). TOFU described first-load as "the host itself considers SHA-256 and trusts the binary as is" - this did not close the substitution of the binary **until** its first appearance on the host (see "Closed gap - first-load" below). With Sigil, the authority over which binary is allowed belongs to the Keeper, not the host.

### Root of Trust

| What | Where |
|---|---|
| **Allow-list** `(namespace, name, ref) → sha256` | PG table `plugin_sigils` (Keeper-state). The entry is added **only when the Archon explicitly allows** the plugin via OpenAPI (`POST /v1/plugins/sigils`, S4a) / MCP (S4b) - permission `plugin.allow`, [rbac.md → Plugin Sigil](rbac.md#plugin-sigil-3). `ref` - **git-verified** (Keeper resolves `source`+`ref` into `commit_sha` cache slot via go-git, [ADR-026(g)](../adr/0026-sigil.md)); chain of trust `ref` → `commit_sha` → `binary_sha256` → Keeper signature. |
| **Keeper signing key** (private) | Vault KV - according to the pattern `secret/keeper/jwt-signing-key` ([ADR-014](../adr/0014-operator-identity.md)). |
| **Keeper public key** (trust-anchor host) | Soul arrives in **bootstrap** along with a CA-chain (the same channel `BootstrapReply` as mTLS CA, [ADR-012(f)](../adr/0012-keeper-soul-grpc.md)): single `sigil_pubkey_pem` or multi-anchor `sigil_pubkey_pem_set` (priority set > single). The runtime set of anchors is delivered to `SigilTrustAnchors` and **completely replaces** bootstrap-anchors (replace, not merge; R3 rotation) - see Active set and replace semantics. |

**Sigil** = `sign_keeper(block)`, where the block being signed carries `(namespace, name, ref, binary_sha256, manifest)`. The signature **covers the manifest** with the attached `binary_sha256` ([ADR-026(c)](../adr/0026-sigil.md)) → declared `side_effects` / `required_capabilities` / `protocol_version` cease to be forged (you cannot replace the manifest without breaking the signature).

> **`ref` - git-verified** ([ADR-026(g)](../adr/0026-sigil.md), **Option A, F-fetch**). Keeper itself resolves `source`+`ref` from the `keeper.yml` directory via **go-git**: shallow `clone`→`fetch`→`ResolveRevision(<ref>^{commit})` (resolved in 40-hex `commit_sha`)→detached-HEAD `checkout`, then extracts the **ALREADY compiled** binary `dist/<binary-name>` + `manifest.yaml` (F-fetch - no compilation on Keeper). Boundary "verified" = "Keeper checked this particular `ref` and recorded the result (`commit_sha` + `binary_sha256`)", **NOT** bit-reproducibility of the assembly. Cache - **R-nested**: `<cacheRoot>/<ns>-<name>/<commit_sha>/` (immutable slot) + symlink `current → <commit_sha>` (atomically permutable pointer to the active slot). **Single-active-per-pair**: `current` points to exactly one `commit_sha`, but multiple `commit_sha` slots under one `(namespace, name)` coexist. `plugin.allow` reads the binary+manifest of the ACTIVE slot via `current` ([`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go)), reads `sha256`, signs and inserts the record; `ref` is not involved in the slot lookup. **Integrity Authority = `sha256` + signature** (invariant (b) ADR-026 not weakened); `ref`/`commit_sha` carry provenance and audit-readability, not trust. `commit_sha` — audit mark OUTSIDE the signature (signature is above `(namespace, name, ref, binary_sha256, manifest_sha256)`); will be added as a column to `plugin_sigils` at S3.

### Signed block format (normative, S3)

The block is assembled with a pure deterministic function (`shared/pluginhost.BuildSigilBlock`) - common code for signature on Keeper (S3) and verification on Soul (S6), **without** proto-marshal (proto-serialization is non-deterministic - it was deliberately excluded):

```
block = DST || LP(namespace) || LP(name) || LP(ref) || LP(binary_sha256) || LP(manifest_sha256)
```

- **`DST`** — domain-separation tag, ASCII constant `soul-stack/sigil/v1` (without length-prefix, fixed known prefix). Version `/v1` is required: change of block format → `…/v2`, old signatures no longer work against the new code (an obvious compatibility gap). DST first → the signature over the Sigil cannot be reused in another protocol.
- **`LP(x)`** = 4 bytes of big-endian uint32 length `x`, then the bytes themselves `x`. Applies to **every** variable field - field boundary protection: without length-prefix, the concatenation of `("ab","c")` and `("a","bc")` would result in one block, and the signature over one set would fit into the other.
- Hashes (`binary_sha256`, `manifest_sha256`) are put in **raw bytes** (for SHA-256 - 32 bytes), **not** a hex string.
- The order of the fields is fixed exactly: `namespace`, `name`, `ref`, `binary_sha256`, `manifest_sha256`.

**Signing key - ed25519** (asymmetry is required, unlike the HS256-symmetric JWT signing-key): the private person lives in Vault KV at `sigil.signing_key_ref` ([config.md → sigil](config.md#sigil)), signature - raw 64 bytes; the public part goes to Soul in bootstrap as a trust-anchor.

**S3↔S6-invariant (manifest bytes).** The `manifest.yaml` bytes that Keeper hashes when signing must match the bytes that Soul re-hashes when verify. Guarantee: (1) manifest and binary are delivered in one artifact stream; (2) both sides run raw bytes through `NormalizeManifestBytes` before SHA-256 - **byte-only** canonicalization (strip BOM, CRLF→LF, exactly one trailing `\n`), **no** re-parse/re-emit YAML (hash should not depend on the version of the yaml emitter).

**Canon vs projection in `plugin_sigils`.** The registry keeps manifest in **two** columns:

- **`manifest_raw` (`bytea`, migration 030) - CANON.** Byte-exact the same raw bytes that are signed (single `ReadSlot` with `plugin.allow`). They are the ones that go to `PluginSigil.manifest` for broadcast and are re-hashed by Soul during verify. The column is nullable at the DDL level (forward-only for old rows), but `allow`-path requires non-NULL: `Insert` rejects empty `manifest_raw` (empty signed bytes = calling bug, root of trust; `{}`-fallback here **not applicable** - `Normalize("{}") != Normalize("")`).
- **`manifest` (`jsonb`, migration 028) - derived.** Projection for query/audit (search by `side_effects` / `required_capabilities`, show in UI). **NOT** canon for verify: JSONB roundtrip does not save bytes. Using `manifest` (JSONB) for verify/broadcast is an error; the source of truth is always `manifest_raw`.

### Mechanism

| Step | Host behavior |
|---|---|
| Discover | Counts SHA-256 binaries streamwise; puts in `Discovered.Digest` for logs / OTel attributes. Error reading binary for digest → plugin skipped with warning. |
| Obtaining a Sigil | **Push** (Keeper transfers plugins FROM Keeper via mTLS - Keeper is already a trust-anchor): Sigil travels with the binary. **Pull** (Soul-demon): Sigil comes only-add proto-message in `EventStream` ([ADR-026(e)](../adr/0026-sigil.md)). |
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
| Cache directory `<cacheRoot>/<ns>-<name>/<commit_sha>/` (R-nested per-commit slot + symlink `current → <commit_sha>`, [ADR-026(g)](../adr/0026-sigil.md)) | `0755` | service-user (`keeper` / `soul`) | Recording is for the owner only. Group/other - read-only, so that an extraneous process does not replace the binary or sidecar. |
| Plugin binary | `0755` | service-user | Executable, writable only by owner. |
| Sidecar `.sha256` | `0400` | service-user | Read-only after recording. |

Host **must** run under a dedicated least-privilege service-user, not root (except for plugins with `run_as_root`-capability). Protection against sidecar spoofing **together** with the binary rests precisely on the rights of the directory: an attacker without write access to the directory will not overwrite either the binary or `.sha256`. Sidecar `.sha256` is a digest cache for re-exec reconciliation (defense-in-depth shared cache), **not** trust-anchor: authoritative source "is the binary allowed" - Sigil (Keeper signature + allow-list `plugin_sigils`), and not a locally calculated hash.

### Closed gap — first-load

The previous TOFU model protected against binary substitution **after** the first loading into the cache, but **not** from a malicious plugin during the **first** loading (if the attacker replaced the binary in artifact-source / during Keeper-checkout of git-ref before the host saw it for the first time) - he went through integrity-gate "as is" and forked with service-user rights (RCE vector). **This gap is closed by Sigil** ([ADR-026](../adr/0026-sigil.md)): first-load is no longer "trust as is" - the host verifies the Keeper's signature and checks the digest with the value **to** seal/exec explicitly approved by the Archon. A Malicious binary without a valid Sigil does not receive control.

> **Implementation (host-side verify - LIVE).** Plugin directory Git resolver ready (A1-S1): [`keeper/internal/plugingit`](../../keeper/internal/plugingit) (go-git F-fetch, R-nested cache `<ns>-<name>/<commit_sha>/` + `current`, scheme-allowlist, git-egress size-limit (ADR-026(g)), sentinels `ErrRefNotResolved`/`ErrManifestNotFound`/`ErrArtifactNotFound`/`ErrSourceUnavailable`/`ErrCloneTooLarge`/`ErrArtifactTooLarge`), config fields `plugins.work_root`/`plugins.fetch_timeout`/`plugins.max_artifact_size_mb`/`plugins.max_clone_size_mb`; reading active slot at `plugin.allow` - [`pluginhost.ReadSlot`](../../keeper/internal/pluginhost/slot.go) via `current`. Keeper-side signature is ready (S3): general block build helper + canonicalization manifest in [`shared/pluginhost`](../../shared/pluginhost) (`BuildSigilBlock` / `NormalizeManifestBytes`), ed25519-signature + CRUD registry `plugin_sigils` in [`keeper/internal/sigil`](../../keeper/internal/sigil), config `sigil.signing_key_ref`. `plugin.allow` persists signed raw bytes in `manifest_raw` (M1-storage, migration 030) - `ListActive` / `GetActive` give them to S6-sender/S6b-verify byte-exact. **Host-side verify-against-Sigil - LIVE (S6):** TOFU branch first-load is replaced by verify by Sigil + multi-anchor set in [`shared/pluginhost`](../../shared/pluginhost) (SHA-256 verification before each `exec` remains defense-in-depth). **Multi-anchor rotation of signature keys - LIVE (R3, [ADR-026(h)](../adr/0026-sigil.md)):** registry `sigil_signing_keys` (migration 037, [`keeper/internal/sigil/keys.go`](../../keeper/internal/sigil/keys.go)), multi-anchor Signer, broadcast `SigilTrustAnchors` + Redis channel `sigil:anchors-changed` (cluster reload), operator-facing rotation (R3-S7: REST `/v1/sigil/keys*` + MCP `keeper.sigil.key.*`, permissions `sigil.key-introduce|retire|list|set-primary`, audit `sigil.key-introduced|retired|primary-set`), bootstrap-reply from live anchor source. Deferred: column `commit_sha` in `plugin_sigils` (A1-S3, audit-label of origin, OUTSIDE signature).

## Versioning

`protocol_version: int` - plugin protocol version. **One field - two places** ([ADR-020(c)](../adr/0020-plugin-infrastructure.md)):

- In `manifest.yaml` - for static `soul-lint`.
- In the handshake line - for runtime sanity before opening gRPC.

### Match `protocol_version` ↔ `proto/plugin/vN/`

| `protocol_version` | proto package | Status | Composition |
|---|---|---|---|
| `1` | `proto/plugin/v1/` | MVP | `handshake.proto`, `manifest.proto`, `soulmodule.proto`, `clouddriver.proto`, `sshprovider.proto` (closing is a separate task after ADR-020). |

### `SupportedProtocolVersions`

Each host binary (`keeper` / `soul` / `soul-lint`) holds a constant - an ordered list of supported protocol versions. In MVP - `[1]`.

Evolution: adding `proto/plugin/v2/` → the next version of the host binary contains `[1, 2]` (forward-compat only-add, analogous to [ADR-012(g)](../adr/0012-keeper-soul-grpc.md) for the plugin protocol). Removing old versions - breaking release of the host binary, separate ADR.

### Cross-check matrix

Summary list of cross-checks between manifest, handshake string and host constant `SupportedProtocolVersions`. Duplicates runtime rows from the table "Host behavior during handshake" in formal notation + adds static check `soul-lint`.

| fail condition | Where | Behavior |
|---|---|---|
| `manifest.protocol_version != handshake.protocol_version` | host, after handshake | Hard fail: drift inside the plugin. |
| `manifest.protocol_version ∉ SupportedProtocolVersions` | `soul-lint` when validating destiny | Destiny validation error **before launch**. |
| `handshake.protocol_version ∉ SupportedProtocolVersions` | host, after handshake | Hard fail: `protocol_version=N, host supports [...]`. |
| `manifest.kind != handshake.kind` | host, after handshake | Hard fail: drift inside the plugin. |

## `required_capabilities`-table

Closed enum capabilities. The plugin declares what it needs from the host system; `soul-lint` checks against `plugin_runtime.allowed_capabilities` host (mismatch → destiny validation error **before launch**).

| Capability | Meaning |
|---|---|
| `run_as_root` | The host process (`soul` / `keeper`) must have UID 0 when running the plugin. |
| `network_outbound` | The plugin makes outgoing network calls (cloud API, vault, package mirror). |
| `network_inbound` | The plugin listens to the port (a rare case for test helper plugins). |
| `vault_access` | The plugin accesses Vault through the client helper SDK. |
| `fs_write_root` | The plugin writes beyond `/var/lib/soul-stack/`. |
| `exec_subprocess` | The plugin runs external commands via `os/exec`. |

Enum expansion is done via PR in `proto/plugin/vN/manifest.proto`, without breaking. Freeform-extensions with the prefix `x-` (open-ended capabilities) **rejected in MVP** - will be added on the first real request.

**Declaration, not runtime-enforcement.** `required_capabilities` is a *static declaration* of what the plugin needs from the host, to check `soul-lint` with `plugin_runtime.allowed_capabilities` host: with mismatch - destiny validation error **before launch**. The host **does not raise rights in this field** - the step is executed exactly with the privileges of the process (`soul` / `keeper`), and the rights field itself does not issue. So, `run_as_root` means "the module only works correctly when the host process is UID 0" (environment requirement), not "the module is promoted to root itself." Built-in core modules (`soul`-side, statically compiled) declare `required_capabilities` in their manifests [`shared/coremanifest/<name>.yaml`](../../shared/coremanifest) and undergo the same** static verification - for them the field does not carry any other (runtime) semantics than for external plugins.

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

manifest parser validates: entry with more than one pair → error `multiple_resource_types_in_side_effect_entry`. Several resources of the **same** type are also written as separate records (for example, two different files - two `{ file: … }` records).

### Host behavior on side_effects

| Situation | Behavior |
|---|---|
| Each resource touched at `Apply` | An entry in audit-event indicating the plugin (`namespace.name`) and `apply_id`. |
| Two plugins in one run on the same resource | According to the `plugin_runtime.conflict_policy` policy: `warn` (default) or `fail`. |
| The plugin touches a resource not from `side_effects` | The step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` (the exact form of the field is a separate audit-pipeline standardization task for `side_effects`, see backlog). |

## Service contract `SoulModule`

Host is a `soul` binary. Binary - `soul-mod-<name>` (for example, `soul-mod-haproxy`).

| Method | Destination |
|---|---|
| `Validate(ValidateRequest) → ValidateReply` | Runtime parameter checks (the full scheme in `manifest.spec.states.<state>.input` has already been checked by `soul-lint`; here are additional semantic checks that require access to the host system). |
| `Plan(PlanRequest) → stream PlanEvent` | Dry-run: the module calculates changes without applying them. Returns the progress event stream. |
| `Apply(ApplyRequest) → stream ApplyEvent` | Applies the changes. Stream events for long-running operations (see the clause about MVP in [ADR-012](../adr/0012-keeper-soul-grpc.md) - progress is aggregated on Soul, only the final result goes out through `TaskEvent`). |

`Manifest()` RPC in MVP **not entered** - the manifest is read by the host from the static `manifest.yaml` ([ADR-020(a)](../adr/0020-plugin-infrastructure.md)). A possible future method (for self-test "compare yourself with the declared one") is in `proto/plugin/v2/`.

Destiny step addressing is `<namespace>.<name>.<state>` (see [`../soul/modules.md`](../soul/modules.md), [naming-rules.md → Destiny Modules](../naming-rules.md)).

## Service contract `CloudDriver`

Host - `keeper` (module `keeper.cloud`, see [`cloud.md`](cloud.md)). Binary - `soul-cloud-<provider>` (for example, `soul-cloud-aws`, `soul-cloud-yc`).

| Method | Destination |
|---|---|
| `Schema(SchemaRequest) → SchemaReply` | Publishes `profile_schema` (JSON Schema of the VM profile; must match `manifest.spec.profile_schema`). Used when creating a Profile via OpenAPI/MCP for validation. `SchemaRequest` is an empty message (instead of `google.protobuf.Empty`) for forward-compat to add fields without breaking change. |
| `Validate(ValidateProfileRequest) → ValidateProfileReply` | Runtime checks of profile parameters (quotas, image availability, subnet validity - things that are not expressed by JSON Schema). The request/reply name is different from SoulModule `ValidateRequest/Reply` - a single proto-package `soulstack.plugin.v1`, message names must be unique. |
| `Create(CreateRequest) → stream CreateEvent` | Creates a VM (one or N), streams progress. |
| `Destroy(DestroyRequest) → stream DestroyEvent` | Deletes a VM. Under guard-rails - see [cloud.md → Security destroy](cloud.md). |
| `Status(StatusRequest) → StatusReply` | Poll the status of a specific VM. `StatusRequest.credentials` carries the plain-secret of the provider (A-flow, symmetrically `CreateRequest`/`DestroyRequest`) - without credentials the driver will not be able to access the provider API. |
| `List(ListRequest) → stream VmInfo` | Enumeration of VMs known to the provider. `ListRequest.credentials` - A-flow (symmetrical `Create`/`Destroy`/`Status`); `ListRequest.filter` - provider-specific filter (tags/region), credentials CANNOT be placed in filter. |

Usage - in [`cloud.md`](cloud.md). Cloud-create is built into scripts as a step with `on: keeper` (module `core.cloud.provisioned`, [ADR-017](../adr/0017-keeper-side-core.md)).

## Service contract `SshProvider`

Host - `keeper` (module `keeper.push`, see [`push.md`](push.md)). Binary - `soul-ssh-<provider>` (for example, `soul-ssh-vault`, `soul-ssh-static`, `soul-ssh-teleport`).

| Method | Destination |
|---|---|
| `Sign(SignRequest) → SignReply` | Issue an SSH certificate/key for the current session (for example, Vault SSH CA issues a short-lived certificate). |
| `Authorize(AuthorizeRequest) → AuthorizeReply` | Confirm Keeper's right to go to a specific host (provider policy, if any). |

This contract covers Vault SSH CA, static-key, Teleport - three candidates for MVP, a specific set of required implementations - [open Q SSH-2 / No. 3](../architecture.md). Usage is in [`push.md → SSH authentication`](push.md#ssh-authentication--pluggable-provider).

## Full manifest examples

### `kind: soul_module` (HAProxy)

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
      description: HAProxy has stopped.
      input:
        name: { type: string, required: true }
    restarted:
      description: HAProxy has been restarted (force-restart).
      input:
        name:        { type: string, required: true }
        config_path: { type: string, default: /etc/haproxy/haproxy.cfg }
    reloaded:
      description: HAProxy reload (SIGHUP) without downtime.
      input:
        name: { type: string, required: true }
```

### `kind: cloud_driver` (AWS)

```yaml
# soul-cloud-aws/manifest.yaml
kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: aws

required_capabilities:
  - network_outbound
  - vault_access

side_effects: []   # cloud_driver does not touch the local resources of the host

spec:
  provider_kind: aws
  profile_schema:
    type: object
    required: [image_id, instance_type, subnet_id]
    properties:
      image_id:      { type: string, pattern: "^ami-[0-9a-f]+$" }
      instance_type: { type: string }
      subnet_id:     { type: string, pattern: "^subnet-[0-9a-f]+$" }
      security_group_ids:
        type: array
        items: { type: string, pattern: "^sg-[0-9a-f]+$" }
      tags:
        type: object
        additionalProperties: { type: string }
```

### `kind: ssh_provider` (Vault SSH CA)

The actual implementation is [`examples/module/soul-ssh-vault/`](../../examples/module/soul-ssh-vault). Vault SSH CA: the plugin goes to Vault itself (variant B, see below), calls `ssh/sign/<role>` to sign Keeper-ephemeral pubkey, returns only `certificate` (`private_key=""`).

**Canonical mode - Keeper-ephemeral** (security-first, PM-decision):

1. `keeper.push` generates an ephemeral ed25519 keypair per-session and passes the public part to `SignRequest.public_key`.
2. The plugin authenticates to Vault (`auth_method: token | approle`), calls `<vault_mount>/sign/<role>` with this pubkey and `valid_principals=<req.user>`.
3. Returns `SignReply{certificate=<signed_key>, private_key=""}` - the private NEVER leaves Keeper.
4. `keeper.push` collects [`ssh.NewCertSigner`](https://pkg.go.dev/golang.org/x/crypto/ssh#NewCertSigner) from ephemeral signer + cert and opens an SSH session. After the session is closed, the private person goes to the GC.

**Creds-flow - variant B** (the plugin itself runs in Vault, [`vault_access` capability](#required_capabilities)): for `ssh/sign` this is more natural than A-flow (Keeper does not act as a proxy to the Vault engine, does not parse the response). Symmetrically for `auth_method: approle` the plugin does `auth/<mount>/login` itself.

**Params** come via env `SOUL_SSH_VAULT_PARAMS` (JSON by `schema.json`, symmetrically by `SOUL_SSH_STATIC_PARAMS`). The SshProvider contract does not carry per-request provider parameters, so the config is sent at the start of the process, like the path to the socket (`SOUL_PLUGIN_SOCKET`).

```yaml
# soul-ssh-vault/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: vault

required_capabilities:
  - network_outbound          # Vault API HTTP calls
  - vault_access              # plugin CAM goes to Vault (variant B)

side_effects: []              # ssh_provider does not touch local resources of the host

spec:
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
# soul-ssh-static/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: static

required_capabilities:
  - vault_access            # optional key resolution from Vault KV

side_effects: []            # ssh_provider does not touch the local resources of the host

spec:
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

### `kind: ssh_provider` (Teleport)

The actual implementation is [`examples/module/soul-ssh-teleport/`](../../examples/module/soul-ssh-teleport). Teleport provider: the plugin goes to Teleport Auth itself (creds-flow B, symmetrically to Vault SSH CA), calls `GenerateUserCerts(SSHPublicKey)` to sign the Keeper-ephemeral pubkey, returns only `certificate` (`private_key=""`) and fills in the only-add field with `SignReply.proxy_jump` endpoint of the Teleport-proxy.

**Canonical mode - Keeper-ephemeral** (security-first, PM-decision): Teleport API (`api/client.GenerateUserCerts`) accepts `SSHPublicKey []byte` and returns a signed SSH-cert for this pubkey - variant A (Vault-style) fits into Teleport without deviating from the key-ownership solution.

**Creds-flow - variant B** (the plugin itself authenticates itself in Teleport): identity-file (`tctl auth sign`) or tbot socket, the path comes to `SOUL_SSH_TELEPORT_PARAMS`. Capability `vault_access` NOT required - Teleport is not Vault; credentials live in a file/socket.

**Dispatcher proxy_jump support - LIVE (S3).** `keeper.push` respects `SignReply.proxy_jump`: if the value is non-empty, the dispatcher opens the SSH client to the proxy hop with the same `cfg.Auth` (signed user-cert on the ephemeral keypair), on this client requests the `direct-tcpip` channel before `host:port` of the target host and performs a second SSH handshake over the channel (equivalent to `ssh -J <proxy> <host>`). Cert from Teleport/Vault SSH CA authorizes the user on both hops - this is canonical Teleport-flow. Host-cert verification (`ssh.CertChecker`, fail-closed without CA) works on BOTH hops: by default, one `HostAuthority.CAPublicKey` is used (typical case - one host-CA signs both proxy and target host-certs); separate proxy-CA - extension via `DialConfig.ProxyHostAuthority` (field without UI for now, activated upon operator request). If `proxy_jump` is empty - direct `net.Dial(host:port)` (S0-flow without regressions). Implementation - [`keeper/internal/push/session.go`](../../keeper/internal/push/session.go) (function `Dial`, branch `dialViaProxy`).

**Params** come via env `SOUL_SSH_TELEPORT_PARAMS` (JSON by `schema.json`, symmetrically `SOUL_SSH_VAULT_PARAMS` / `SOUL_SSH_STATIC_PARAMS`).

```yaml
# soul-ssh-teleport/manifest.yaml
kind: ssh_provider
protocol_version: 1
namespace: ssh
name: teleport

required_capabilities:
  - network_outbound          # gRPC calls to Teleport Auth API via Teleport-proxy

side_effects: []              # ssh_provider does not touch the local resources of the host

spec:
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
kind: soul_beacon
protocol_version: 1

namespace: community
name: zfs-degraded

required_capabilities:
  - exec_subprocess           # launch `zpool status`

side_effects: []              # read-only, beacon does not mutate the host

spec:
  params_schema:
    type: object
    required: [pool]
    properties:
      pool: { type: string }  # ZFS pool name to poll
```

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

### What Keeper does as a resolver (git-verified, F-fetch)

Keeper resolves the directory itself at startup - via [`keeper/internal/plugingit`](../../keeper/internal/plugingit) ([ADR-026(g)](../adr/0026-sigil.md), A1-S1). For each entry:

1. `validateGitScheme(source)` — scheme-allowlist: prod `https://` / `ssh://` / scp-form `user@host:path`; `file://` - only under the env flag `SOUL_STACK_ALLOW_FILE_REPOS=1` (dev/test). Different scheme → `ErrSourceUnavailable`.
2. shallow `clone` (`Depth=1`) working clone in `<work_root>/<name>/` (STRICTLY outside `cache_root`), or `fetch` if there is already a clone. Transport - **go-git** (pure-Go, without system fork `git`); auth - SSH agent for ssh/scp forms.
3. `ResolveRevision(<ref>^{commit})` → 40-hex `commit_sha` (candidates: tag → remote-tracking-branch → full hash; unresolved → `ErrRefNotResolved`).
4. detached-HEAD `checkout` on `commit_sha` (does not execute go-git hooks).
5. parse `manifest.yaml` checkout (no → `ErrManifestNotFound`) → by `kind` → `dist/<binary-name>` convention (the binary is already built, **F-fetch - Keeper does not compile**; no / not a regular file → `ErrArtifactNotFound`).
6. atomic-extract manifest+binary into immutable slot `<cache_root>/<ns>-<name>/<commit_sha>/` (staging on the same fs → fsync → `rename`); `commit_sha`-slot is immutable (re-resolving the same commit - skip).
7. atomic switching symlink `<cache_root>/<ns>-<name>/current → <commit_sha>`.
8. `binary_sha256 := sha256(<slot>/<binary-name>)`.

Per-entry resolve **fail-closed**: broken entry (any sentinel above / unreachable remote / timeout) → per-entry warning, Keeper does not crash. During apply operations, the plugin is launched from the active slot (`current`).

git stack - go-git by-design: hooks are not executed, submodules are not recursive, `ext::` is missing, `file://` is locked by scheme-allowlist; **no runtime dependency on the `git`** binary on keeper-host. Hardening: `Depth=1` + context-timeout (`plugins.fetch_timeout`, default 120s), `plugins.work_root` STRICTLY outside `cache_root`, **size-limit by volume** (`plugins.max_clone_size_mb` for the clone working tree + `plugins.max_artifact_size_mb` for the binary, fail-closed - see below). git-egress - **HIGH security risk**: the remainder of the mandatory security pass before proceeding (`noexec` per slot / sandbox of git operations) - postponed. **GC of old `commit_sha`-slots** (several slots per pair after `ref` rotations/commits) - deferred, candidate for Reaper rule (name - separate propose-and-wait).

**Size-limit (ADR-026(g), fail-closed).** `source` operator-asserted, but the repository is untrusted, and `fetch_timeout` limits egress only in time. Two caps in size protect the keeper-host disk from DoS by a hostile/huge repo:

- `plugins.max_clone_size_mb` (default 1024 MiB) - the total size of the clone working tree (du-like walk checkout + `.git`), checked **after checkout, before extracting the artifact**. Excess → `ErrCloneTooLarge` + cleanup `work_root/<name>`.
- `plugins.max_artifact_size_mb` (default 256 MiB) - binary size `dist/<binary-name>`, checked against `os.Stat` before copying and `io.LimitReader` during copy (defense-in-depth). Excess → `ErrArtifactTooLarge`, slot does not materialize.

Both sentinels are per-entry fail-closed (warning, like `ErrArtifactNotFound`/`ErrSourceUnavailable`): the broken entry is skipped, the slot is not created → the plugin has **nothing to allow** through Sigil.

Resolver Config fields in [`config.md → plugins`](config.md):

| Field | Default | Meaning |
|---|---|---|
| `plugins.cache_root` | `pluginhost.DefaultCacheRoot` | Root of the R-nested slot cache (`<ns>-<name>/<commit_sha>/`). Absolute path. |
| `plugins.work_root` | `/var/lib/soul-stack-keeper/plugin-src` | The root of the resolver's working git clones. **STRICTLY outside `cache_root`** (`.git`/checkout does not go into the readable cache directory). Absolute path. |
| `plugins.fetch_timeout` | `120s` | The ceiling of one chain of go-git resolve operations (clone→fetch→checkout). git-egress - external call, timeout required. |
| `plugins.max_artifact_size_mb` | `256` | Binary size ceiling `dist/<binary-name>` (size-limit hardening). Excess → `ErrArtifactTooLarge`, fail-closed. |
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
- [naming-rules.md](../naming-rules.md) — `Manifest`, `Handshake`, `kind`, `SoulModule`, `CloudDriver`, `SshProvider`, capabilities-enum, resource-types-enum.
