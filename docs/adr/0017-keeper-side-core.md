# ADR-017. Keeper-side core modules extended: `core.cloud.provisioned`, `core.vault.kv-read`

> ★ **`examples/service/redis` is no longer in this repository.** It left with NIM-871 and lives on its own at [`soul-stack-services/redis`](https://github.com/soul-stack-services/redis). Every citation of that path below names a decision and the shape it took, not a file you can open here.

- **Context.** The precedent of keeper-side core (`core.soul.registered`, [`docs/keeper/modules.md`](../keeper/modules.md)) demonstrated a pattern: the module executes on the keeper instance (dispatcher `on: keeper`), operates on Postgres/Redis registries, has the same `<namespace>.<module>.<state>` contract as Soul-side core. In the existing examples (`service.yml`, [Service — structure and manifest](../architecture.md#service---structure-and-manifest)) a destiny `cloud-provision` with `on: keeper` figures. This is a semantic drift: a destiny is a package of tasks for a Soul, not a keeper-side operation. Similarly, the "vault-resolve phase" in [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files) is currently implicit — it is worth formalizing it as an explicit module step, so that audit/RBAC/OTel work uniformly.
- **Decision.** Two new keeper-side core modules are introduced:

  **(a) `core.cloud.provisioned`.**
  - State forms: `created` / `destroyed`.
  - Purpose: creating / deleting a VM or another cloud entity via a `CloudDriver` plugin (`soul-cloud-*`, [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules), [Plugin infrastructure](../architecture.md#plugin-infrastructure)).
  - Step addressing in a scenario: `module: core.cloud.created` (or `core.cloud.destroyed`), `on: keeper`, `params:` contains `provider` / `profile` / `count` / other cloud parameters. (The historical form `module: core.cloud.provisioned` in the early text of this ADR — is NOT an existing address; see amendment (u)/(v) 2026-06-26.)
  - Replaces the old "destiny `cloud-provision`" pattern — the corresponding examples in `service.yml` are updated (a separate task).
  - Output (register-payload): a list of the created hosts with their `sid` / `coven[]` / cloud metadata. Via `register:` and [`soulprint.hosts`](../scenario/orchestration.md) it is available to the subsequent scenario steps.

  **(b) `core.vault.kv-read`.**
  - State form: `read` (a verb).
  - Purpose: reading a Vault KV secret on the keeper side at the moment of scenario render. Replaces the implicit "vault-resolve phase" ([ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)) as an explicit module step for cases where the operator wants an explicit audit of the read.
  - Addressing: `module: core.vault.kv-read`, `on: keeper`, `params: { path: secret/redis/admin-password, fields: [password] }`.
  - Output: the requested fields as a register-payload (secret-masking — on output via the standard mechanism, see [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files)).
  - Implicit vault-resolve in CEL expressions (`${ vault('secret/.../password') }`) — **remains** for convenience; the explicit module is for cases where an explicit task-step is needed.

  **(c) Addressing and the contract are identical to Soul-side core** — deliberately. Authors of keeper-side custom modules (in perspective) implement the same SoulModule interface from `sdk/module/`.

- **Consequences.**
  - In [`docs/keeper/modules.md`](../keeper/modules.md) the specifications of the two new modules are added.
  - In the `service.yml` examples and the destiny catalog the "destiny `cloud-provision`" pattern is replaced with the "scenario-task `module: core.cloud.created` with `on: keeper`" — this is work for a separate examples-migration task.
  - In [naming-rules.md → section "Specific core modules"](../naming-rules.md) the names are added.
  - **Cascade `destroyed`-state** (migrations 016+017): after a successful `PluginHost.Destroy` the module `core.cloud.provisioned`, in a single PG transaction, moves `souls.status → 'destroyed'` (a terminal-state, an enum extension), the active `soul_seeds.status → 'orphaned'` (a new status; `revoked` is NOT overwritten — precedence revoked > orphaned), the not-yet-used `bootstrap_tokens` → `used_at = NOW(), used_by_kid = 'system-cloud-destroy'` (an anti-replay invariant, a special marker outside the format of a real KID). `destroyed` is deliberately NOT part of the default-set `purge_souls.statuses` (forensic > GC). The full semantics — in [`docs/soul/identity.md`](../soul/identity.md).
  - Open Q #5 — partially closed ([ADR-015](0015-core-modules-mvp.md#adr-015-core-modules-mvp-exact-list) + this one).
- **Trade-offs.**
  - Duplication (implicit vault-resolve + explicit `core.vault.kv-read`) — two paths for one task. Accepted: the implicit one is cheap for render, the explicit one is needed for audit (a destination step in a scenario). Each for its own use case.
  - The existing examples with a destiny `cloud-provision` remain temporarily invalid until the migration is done — an acceptable intermediate state, marked in [open questions].

- **Amendment (2026-05-25, cloud credentials-flow + 6 official providers).** The `core.cloud.provisioned` → `CloudDriver` contract is augmented with the mechanics of passing credentials and the first set of providers is fixed. Closes [open Q №13](../architecture.md#open-questions) (the list of providers) and fixes the recommended path for [open Q №14](../architecture.md#open-questions) (bootstrap of the soul agent on a VM).
  - **(d) Credentials-flow — Variant A (Keeper resolves, the driver does NOT go to Vault).** `core.cloud.provisioned` reads the Provider registry (Postgres), resolves `credentials_ref` from Vault on the keeper side and puts the **plaintext credential** into `CreateRequest.credentials` (a provider-specific `google.protobuf.Struct`; `region` — inside the struct, since **Proxmox / OpenStack** have no region as such — the field is optional per-provider). The driver (`soul-cloud-*`) **does not go to Vault** — the capability **`vault_access` is removed** from the cloud manifest (the driver does not need a Vault client). The vault-ref is masked in errors via `audit.MaskSecrets`. Symmetric to the keeper-side resolution of secrets from [ADR-027(f)](0027-apply-work-queue.md#adr-027-apply-execution-model--work-queue--claim-acolyte-pool-ward-claim) (secrets are revealed on the keeper, not in the plugin). Alternative B (the plugin goes to Vault itself, as in the early cloud manifest) — rejected for cloud (Keeper is already authority over the Provider registry; less trusted code in the plugin). For SSH providers (`ssh/sign`) the A/B choice is still open — see [Open questions](../architecture.md#open-questions).
  - **(e) only-add proto `proto/plugin/v1/clouddriver.proto`.** Added are `CreateRequest.credentials` + `CreateRequest.userdata` (a cloud-init blob, see (g)) and `DestroyRequest.credentials`. only-add within `proto/plugin/v1/` ([ADR-020(c)](0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle) forward-compat — never-reuse field numbers). Deferred pre-rollout: `Status` / `List` must also accept a credential (only-add `StatusRequest.credentials` / `ListRequest.credentials`) — introduced before the implementation of the corresponding methods.
  - **(f) Shared SDK skeleton (`sdk/clouddriver`).** The common (not provider-specific) part of the driver is extracted into the SDK: an **error taxonomy** (classification of cloud errors into retryable / terminal / auth), **Retry** (backoff), **WaitUntilReady** (anti-orphan — the driver waits until the VM transitions into running/ready, so as not to return a half-created resource without tracking) and **ConfirmDestroy** (the mirror image on teardown — the driver waits until the provider reports the VM actually gone, see the amendment 2026-07-26/NIM-191). The provider-specific part of each driver is minimal — this is the **rollout gate** (the pilot sets the pattern, the rest replicate it, see [CLAUDE.md → Mass operations](../../CLAUDE.md)).
  - **(g) Six official providers (closes [open Q №13](../architecture.md#open-questions)).** The first set includes: **AWS / GCP / Azure / Yandex Cloud / Proxmox / OpenStack**; the binaries — `soul-cloud-aws` / `soul-cloud-gcp` / `soul-cloud-azure` / `soul-cloud-yc` / `soul-cloud-proxmox` / `soul-cloud-openstack` (the Yandex Cloud name is shortened to `yc`). **vSphere** — community / deferred (not in the first set). **AWS — the pilot** (the reference implementation on which the pattern is fixed before rolling out the other five). The rollout stop-rule — the shape of the provider-specific credentials-struct and WaitUntilReady on-prem (Proxmox without a region, OpenStack Keystone-auth).
  - **(h) Bootstrap of the soul agent on a VM — the recommended path is cloud-init userdata (per [open Q №14](../architecture.md#open-questions)).** The main path — **cloud-init userdata**: `CreateRequest.userdata` carries a blob that deploys the soul agent on the new VM. **A per-VM-token-in-userdata is deferred** (chicken-egg: the SID is assigned **after** create — at the moment of forming the userdata there is no individual token for the specific SID yet). In MVP — a **common batch-blob userdata** + onboarding via CSR ([ADR-012](0012-keeper-soul-grpc.md#adr-012-keepersoul-grpc-contract-one-eventstream-with-oneof-keeper-side-render-forward-compat-only-add) `Bootstrap`, the private key does not leave the host). Alternative paths (`keeper.push` after create / a gold image with soul in the AMI) remain possible, but cloud-init is the recommended default for cloud-create.
  - **(i) Deferred pre-rollout (edge-hardening).** In addition to `Status` / `List` credentials (see (e)): a driver-side **wait-deadline** (a hard timeout for waiting for ready), a **terminal-state probe** (recognizing a failed create instead of infinite waiting), a **partial-batch** (correct handling of `count>1` when part of the VMs were created and part were not). Introduced before the batch rollout of providers.

- **Amendment (2026-05-26, cloud parity implemented — 6 providers).** The rollout by the AWS-pilot pattern (amendments (d)-(i) above) is finished; **6 official CloudDriver plugins are committed and working**, closing (g) — the MVP provider list — with the actual implementation.
  - **(j) The committed cloud plugins.** The plugins live in the core repo `examples/module/soul-cloud-*/` (NOT in the companion `soul-stack-plugins` — that one contains only the SoulModule official set).
    - **AWS** — commit `ec83487` (pilot, reference). Edge-hardening (wait-deadline / terminal-state probe / partial-batch / `Status`/`List` credentials, (i) and (e)) — commit `72ff14b`. On AWS the pattern for the rollout is fixed.
    - **GCP** — commit `6bfd83c` (batch-1, hyperscaler-like).
    - **Yandex Cloud (`soul-cloud-yc`)** — commit `a181d49` (batch-1, hyperscaler-like).
    - **Azure** — commit `c7d93ad` (batch-1, hyperscaler-like; a multi-resource per-VM flow — NIC + a public IP + a disk).
    - **OpenStack** — commit `a03a404` (batch-2, divergent: Keystone v3 application credentials / project-scoped tokens instead of a single static API-key).
    - **Proxmox** — commit `af43665` (batch-2, divergent: a VM-clone from a template instead of an image, a composite `vm_id = node:vmid` instead of a single string, no region).
  - **(k) Architectural conclusions of the rollout.**
    - **credentials-flow Variant A worked on ALL 6** — including Keystone v3 (OpenStack) with the multi-level structure `{auth_url, project_id, user_domain, project_domain, username, password}`; everything fit into a flat `google.protobuf.Struct` without introducing a provider-specific wrapper-message. Alternative B (the plugin goes to Vault itself) for cloud is finally rejected — Variant A is empirically adequate.
    - **the shared SDK skeleton (`sdk/clouddriver`) was reused by all 6 without changes** — `Classify` / `FailClass` (the error taxonomy), `Retry` (backoff), `WaitUntilReady` (anti-orphan) handled both the hyperscaler-different (AWS DescribeInstances vs GCP `instances.get` vs Azure `GET /providers/.../virtualMachines/.../instanceView`), and the on-prem (OpenStack `GET /servers/{id}` / Proxmox `qm status`). **The rollout gate worked** — the pilot set the pattern, the other 5 fit into it.
    - **only-add proto (`CreateRequest.credentials/userdata` + `DestroyRequest.credentials` + `StatusRequest.credentials` + `ListRequest.credentials`)** — all 6 RPCs correctly carry credentials, no provider required extending the contract beyond the declared fields (i)/(e).
    - **`vault_access` capability removed from all 6 cloud manifests** ((d)) — actually done in each release.
    - **The rollout stop-rule did not fire on any** — even on the divergent ones (Azure multi-resource + OpenStack Keystone v3 + Proxmox composite `vm_id`). This means: the pattern was chosen correctly (neither over-generalized nor under-generalized), and for each failed assumption a local solution was found in the `Provider`-specific code without editing the SDK / proto / Keeper-side.
  - **(l) [open Q №13](../architecture.md#open-questions) — closed.** The MVP provider list is not only fixed (amendment (g) 2026-05-25), but also **fully implemented** by 2026-05-26. vSphere remains community / deferred per decision (g).

- **Amendment 2026-05-27 (cloud-init bootstrap MVP, B-flat fixed).** Refines (h) to match the actual code. The previous text "a per-VM-token is deferred (chicken-egg)" is **outdated**: the implemented code `keeper/internal/coremod/cloud/provisioned.go::applyCreated` already issues per-VM bootstrap tokens AFTER the Create phase (when SID = FQDN of the VM is known from `VmInfo.fqdn`), storing them in `register.<step>.hosts[i].bootstrap_token` (see the inline comment "security, H1"). This is the **B-flat** variant: the per-VM token exists, but is delivered to the VM **not through cloud-init userdata**, but **by a separate scenario step** (typically `keeper.push` via an SSH provider). The cloud-init userdata CARRIES ONLY: installation of the `soul` binary (curl with a pinned-CA), the embedded PEM CA of the Keeper (`/etc/soul/tls/keeper-ca.pem`), a minimal `soul.yml` with `keeper.endpoints` (LB host:port), the systemd unit `soul.service`. **WITHOUT tokens** — the cloud-provider API stores userdata in plaintext metadata accessible to VM processes (the security floor). A late-binding callback `/v1/bootstrap-tokens/claim` for self-bootstrap in a single cloud-init cycle — deferred post-MVP (requires a new token-kind, an endpoint, a separate TOFU mitigation).
  - **(m) Implementation.** The package [`keeper/internal/cloudinit`](../../keeper/internal/cloudinit/): `Config` + `GenerateUserdata(cfg)` renders the cloud-config YAML via the `go:embed` template `templates/cloud-init.tmpl`; `Resolver.Resolve(ctx, cfg)` pulls the CA PEM from Vault by `tls_ca_ref`. An optional block `keeper.yml::cloud_init` (a new field `KeeperConfig.CloudInit` in `shared/config`): `bootstrap_endpoint` (LB host:port), `tls_ca_ref` (`vault:<mount>/<path>`, field `ca`), `soul_binary_url` (HTTPS, verified), `soul_version` (an optional label). Hot-reload works via `config.Store.Get()` — the next cloud-create step will pick up the updated snapshot without a restart.
  - **(n) Scenario contract.** The `core.cloud.provisioned` parameter `state: created`: `generate_userdata: true` (bool, optional, default false) — renders userdata from keeper.yml::cloud_init. Mutually exclusive with an explicit `userdata: "<blob>"` (simultaneous presence → fail with a clear error). Without `generate_userdata` and without `userdata` — the provider receives empty userdata (legacy behavior, convenient for the gold-image flow).
  - **(o) Per-provider templates and Ignition — deferred.** MVP: a single common cloud-config YAML for all 6 providers. CoreOS/Flatcar Ignition (the `#!/ignition` JSON format) — a separate slice on request. soul-binary signature verification (sigstore / detached signature) — a separate slice.
  - **(p) Closes the recommendation for [open Q №14](../architecture.md#open-questions)** for the typical cloud-create flow. Alternative paths (`keeper.push` after create without cloud-init / a gold image with soul in the AMI) remain possible.

- **Amendment (2026-06-22, transparent KV v1/v2 support — the version is resolved by a probe, not a heuristic).** Refines (b) `core.vault.kv-read` and all keeper-side Vault KV access to match the actual code `keeper/internal/vault/client.go`. Affects all ~15 callers of `Vault.ReadKV`/`WriteKV` (bootstrap / sigil / push / cloudinit / cloud-credentials / `core.vault.kv-read` / CEL `${ vault(...) }` / augur / herald / toll) — the surface is transparent, the public method signatures have not changed.
  - **(q) The KV mount version is resolved in `vault.Client`.** `kv_mount` now supports both KV v1 and KV v2; the version is determined by a lazy thread-safe probe `sys/internal/ui/mounts/<mount>` (reads `data.type == "kv"` + `data.options.version` ∈ {"1","2"}), the result is cached per-mount. `ReadKV`/`WriteKV` are routed to `KVv1`/`KVv2` respectively; the `ReadKV` return contract — a single flat payload (secret fields) for both versions. The probe is **strictly lazy** (the first real `ReadKV`/`WriteKV`), NOT in the constructor — otherwise the bootstrap path would break (`keeper init` raises the Client before granting KV access, the constructor does only a `Ping` on `sys/health`).
  - **(r) Explicit override `vault.kv_version`.** A new optional field `keeper.yml::vault.kv_version` (`"1"`/`"2"`; empty → auto). The override wins over the probe **without a round-trip** — needed when the ACL closes the probe endpoint. Additive (old configs work). `ListKV`/`ReadKVMetadata` — v2-only (KV v1 has no `<mount>/metadata/` path); on a v1 mount they return a clear error "requires KV v2," rather than a silently broken path.
  - **(s) Fail-closed on an indeterminable version.** If the probe did not yield an unambiguous version (no `options.version` / an unexpected value / `type != "kv"` / permission-denied without an override) — an **explicit error** with a hint about `vault.kv_version`, and NOT a silent default of v2.
  - **(t) The previous mechanism "a discriminator by the error class of `KVv2.Get`" is REJECTED.** Early on it was assumed to read KV transparently via "try v2, on `ErrSecretNotFound` fall back to v1." It failed empirically: **an ordinary v1 secret is indistinguishable from a v2-missing** — `KVv2.Get` on a v1 mount returns the same `ErrSecretNotFound` as a genuinely absent v2 secret, so valid v1 secrets were NEVER read, and a v2-missing risked being masked as a v1 read. The probe mechanism holds this invariant **by construction** (the version is known before the round-trip), not by a heuristic. A latent companion: `core.vault.kv-read` previously ran the payload through `extractKVData`, which expected a nonexistent `{data,metadata}` wrapper (`ReadKV` returns a flat payload anyway) — removed.

- **Amendment (2026-06-26, alignment of the author-facing cloud module address — `core.cloud.created` / `core.cloud.destroyed`, not `core.cloud.provisioned`).** Refines the spelling of the task address in `module:` to match the actual registry (`keeper/internal/coremod/registry.go`) + code (`keeper/internal/coremod/cloud/provisioned.go`) + the integration test + the canon [naming-rules.md → `core.cloud`](../naming-rules.md). **ONLY the spelling of the module address changes** — the state forms (`created`/`destroyed`), the `CloudDriver` contract, the credentials-flow (Variant A), the cascade `destroyed`-state and the cloud-init bootstrap from amendments (a)–(t) above are **not affected**.
  - **(u) The real author-facing address — the base `core.cloud` + a state suffix.** The registry holds **one** keeper-side module named `core.cloud` (`const Name = "core.cloud"` in `provisioned.go`); the task address in `module:` the operator writes as `core.cloud.created` / `core.cloud.destroyed` (+ `core.cloud.resized` — auto-expansion of a VM). `config.SplitModuleAddr` splits the address in `keeper_dispatch` into `(base, state)` = (`core.cloud`, `created`) and so on. The form **`core.cloud.provisioned` as a task address does NOT exist**: `provisioned` is an unknown state that the integration test catches as a fail (there is no such state form in the `core.cloud` manifest → `{created, destroyed, resized}`).
  - **(v) "provisioned" in this ADR — a historical wording + the name of the Go package.** The previous text (the original "Decision" and amendments (a)/(d)/(m)/(n)) used `core.cloud.provisioned` as a step address — this is **historical prose**, written before the base+state split was fixed. The word "provisioned" legitimately remains as: the name of the Go package (`keeper/internal/coremod/cloud/provisioned.go`), the module file name, a reference to the module entity in prose ("the module `core.cloud.provisioned`"). Wherever in this ADR `module: core.cloud.provisioned` was used as a **task address** in a scenario — read it as `core.cloud.created` / `core.cloud.destroyed` (the inline occurrences are brought to the canonical form further in the text). The canon of the author-facing address — in [naming-rules.md](../naming-rules.md), this amendment merely removes the drift within ADR-017.

- **Amendment (2026-06-28, `core.vault.kv-present` — generate-if-absent for secrets).** Extends (b): to the state `kv-read` a second state `kv-present` is added on the **same** module `core.vault` (`const Name = "core.vault"`, the registry holds one module per base name, the state dispatch is inside `Validate`/`Apply` — the `core.cloud`/`core.choir` pattern). The author-facing task address — `core.vault.kv-present` (`SplitModuleAddr` → (`core.vault`, `kv-present`)). Closes the need for scenarios "generate a password if it does not yet exist" without a manual `vault kv put` by the operator (typically — bootstrap secrets of a service at the first `create`).
  - **(w) Semantics — generate-if-absent, idempotent.** For each target the module reads the Vault path: the field is absent/empty → it generates a cryptographically random value and writes it (`WriteKV`), the field exists → no-op. `changed=true` only if something was actually generated (like `core.soul.registered`); a fully-present run → `changed=false`, an audit event is NOT written. The write is a **read-merge-write**: generation into a path with existing neighboring fields preserves them (the new KV version carries both the old fields and the generated one). Several targets on one path are merged into a single `WriteKV`. **Idempotency** — a repeated run of the same step changes nothing. Vault KV v1/v2 — transparently via `vault.Client` (amendment (t), the probe version resolution).
  - **(x) A declared password-policy (a requirement: the service author declares the generation rules).** Params: `targets` (required, a non-empty `[{path, field?, policy?}]`; `field` default `password`) + a step-level `policy` (a common default for targets without their own). **policy** = `length` (the length of the resulting string **in characters**, not bytes of entropy — predictable for the author; default 32, bounds 8..1024) + an **alphabet** in one of two forms: a named `charset` (`alphanumeric` / `hex` / `base64url` / `ascii-printable-safe`) OR an explicit `allowed_chars` (a string of allowed characters; mutually exclusive with `charset` — specifying both is an error). **The default alphabet — `ascii-printable-safe`** = printable ASCII `0x21..0x7E` minus `space " ' # \` `` ` `` `$` (which break redis.conf / users.acl / provoke interpolation) — the default password does not break the target config. A per-target `policy` overrides the step-level one. Generation — **`crypto/rand`** (not `math/rand`), a bias-free character selection (`rand.Int` over the alphabet length — uniform, without a modulo skew).
  - **(y) Security — the generated value does not leak outward (the invariant of [ADR-010](0010-templating.md#adr-010-templating-engine-cel-for-yaml-expressions-go-texttemplate-for-files), the reference `sigil.KeyService.Introduce`).** The register-output carries ONLY `generated` = a map `<path>` → a sorted list of the **names** of the generated fields (without values). The audit event **`vault.kv-present`** (`source: keeper_internal`, written only when `changed=true`) — the payload `{paths}` = the same map path → fields, without values. The new value itself does not get into output / audit / log / OTel; errors of `WriteKV`/`ReadKV` carry only the path (the vault.Client invariant). A guard-test (`kvpresent_test.go::TestPresent_SecurityNoLeak`) recursively checks for the absence of the value (including as a substring) throughout the entire output and payload tree.
  - **(z) Boundaries.** The module does NOT rotate and does NOT delete secrets (it only creates missing ones); the cloud `destroyed`-cascade (amendment (a)) does NOT clean Vault KV — secrets survive the destroy of the incarnation (forensic > GC, like `souls.status='destroyed'`). CEL `${ vault(...) }` (read-only) and the render `vault_resolve` are not affected. A per-secret policy "if the value exists but does not conform to the policy" (re-generate on non-conformance) — is NOT implemented (present == a non-empty field, without a format check).

- **Amendment (2026-06-29, profile semantics — Variant A, Keeper resolves name→params).** The `profile` param of the `core.cloud.created` step = the **NAME** of a row in the `profiles` registry (`/v1/profiles`, migration 020), and **NOT an inline object**. Keeper resolves the name into VM-spec params via `Resolver.ResolveProfile` (`keeper/internal/coremod/cloud/credentials.go`), symmetric to `provider`→credentials (Variant A above). An inline-object profile in `params:` is **NOT supported** — a vestige of the early design before the Profile registry appeared; no service used it (both — `example-cloud-bootstrap` and redis — sent a name). The profile must be pre-registered via `POST /v1/profiles` before running the provision. Closes the drift: the code + `docs/keeper/cloud.md` §31 (removed in NIM-761) declared an object, both services sent a name — there was no resolution, the provision never ran live. The synchronization of `docs/keeper/cloud.md` §31 (removed in NIM-761) object→string is done by the docs-writer separately.

- **Amendment (2026-07-01, self-onboard "Variant T" — per-VM tokens baked into userdata; fixed retrospectively 2026-07-03).** Extends (h)/(m)/(n): a third bootstrap-delivery mode — the VM onboards **itself in a single cloud-init cycle**, without a subsequent push step (`core.bootstrap.delivered`, [ADR-063](0063-bootstrap-token-delivery.md)) and without a claim-callback. Implemented and committed 2026-07-01 (SESSION-A); the retro-fixing — paying off the debt of NIM-18.
  - **The chicken-egg (h) is removed by predicting the FQDN.** The previous blocker "the SID is assigned AFTER create — a per-VM token in userdata is impossible" is circumvented: the keeper **itself sets** the base name of the VM batch (a new opt parameter `name` of the `core.cloud.created` step, `CreateRequest.name`; the driver names the VM `<name>-<index>`) and knows the **provider's FQDN suffix** (a new Provider-registry field `fqdn_suffix`, migration 094; a function of namespace+cluster, e.g. `<namespace>.vm.<cluster>`) → the full FQDN of each VM (`<name>-<index>.<fqdn_suffix>`) is predictable BEFORE create. A provider without a predictable FQDN does not support self-onboard (an explicit error).
  - **Scenario contract.** `self_onboard: true` (bool, opt) on `core.cloud.created`: requires `name`; mutually exclusive with an explicit `userdata:` (the keeper must bake the tokens itself); `generate_userdata` under self-onboard is implied. Before create the keeper issues per-VM bootstrap tokens for the predicted SIDs, inserts `souls`+`bootstrap_tokens` and renders the userdata with a map FQDN→token (`GenerateUserdataSelfOnboard`; the file `/etc/soul/self-onboard-tokens`, 0600 — cloud-init selects its own line by `$(hostname -f)` and does `soul init` between installing the binary and `soul run`). **A plain token is NOT put into the register-output** (there is no delivery — the B-flat key `bootstrap_token` is absent in this mode).
  - **Orphan-cleanup (rollback).** The souls/tokens are inserted BEFORE create → a create/verification failure rolls them back (`SoulStore.DeleteBySID` / `TokenStore.DeleteByTokenID`), otherwise a rerun would hit a PK conflict.
  - **★ A deliberate deviation from the security-floor B-flat (amendment 2026-05-27).** B-flat kept userdata "WITHOUT tokens" (the cloud-provider stores userdata in plaintext-metadata accessible to VM processes). Self-onboard T puts the tokens into userdata deliberately: the tokens are **single-use** (redeemed immediately on the first boot cycle, reuse is impossible), the alternative — a mandatory push-delivery, which some platforms lack.

  > **Correction (2026-09-19, NIM-865, [ADR-0090](0090-bootstrap-reply-loss-recovery.md)):** "reuse is impossible" is no longer literally true, and this trade-off is restated rather than withdrawn. A burned token may be presented again — but only carrying the **same public key** the first presentation bound, on a host that has **never opened a stream**, and the Keeper verifies the CSR's self-signature, so a re-presentation requires the **private key**. That key lives at `0400` under `paths.seed`, which the userdata reader this paragraph is worried about (any VM process, via the metadata service) does not have. The property this argument actually needs — *reading the userdata does not get you an identity* — therefore still holds, and rests on possession of the key rather than on the token being spent. The mode choice is up to the operator per-step (`self_onboard` opt-in); B-flat remains the default. The symmetry of the trade-off "confirmed by the user" [ADR-064](0064-secret-write-path.md).
  - **Boundaries.** On platforms with userdata disabled (`ci_user_data` off, [ADR-066](0066-teleport-onboarding-profile.md)) the mode is unavailable — there the regular path is a full-install via Teleport ([ADR-063](0063-bootstrap-token-delivery.md)); when a provider enables userdata, self-onboard T works without code changes.

- **Amendment (2026-07-01, cert-rotation via Warrant — Keeper-central; fixed retrospectively 2026-07-03).** The user's decision 2026-07-01: the direction "Var1 Keeper-central" is accepted, the registry name — **Warrant**, the R2 deviation confirmed. The contour of auto-rotation of the **SERVICE** TLS certs of incarnations (e.g. the server TLS of Redis, which the `rotate_tls` scenario sets up) — NOT the identity certs of a Soul (`soul_seeds` are not affected). The foundation is implemented (migrations 092/093, `keeper/internal/cert`, `keeper/internal/coremod/cert`, `keeper/internal/reaper/rotate_certs.go`); live validation of the full chain — pending.
  - **Warrant — a PG registry of the issued service certs** (table `warrant`, migration 092): `cert_id` PK, `incarnation_id` FK CASCADE, `kind` (`cert`/`key`/`ca`), `vault_ref`, `serial_number`, `fingerprint`, **`not_after` (the scan axis)**, `status` (`active`/`superseded`/`expired`/`rotating`/`failed`), `auto_rotate`, `rotate_threshold_override`; one active per `(incarnation, kind)` (partial-unique, symmetric to `soul_seeds`). The name was chosen by the user (propose-and-wait passed; the alternatives `Vellum`/`Aegis` rejected); entered into [naming-rules.md](../naming-rules.md).
  - **★ R2 — a deliberate exception from the identity invariant (confirmed by the user 2026-07-01).** Keeper generates the private key of the service cert **centrally** (keeper-side genkey+CSR → Vault PKI `SignCSR` → `WriteKV`) — the private key passes through Keeper→Vault. The invariant "the private key never leaves the host" for **identity** (SoulSeed, [ADR-012](0012-keeper-soul-grpc.md)) is NOT weakened: a service cert ≠ an identity cert, and it already lies in Vault for the manual `rotate_tls`. In PG the private key is not stored — only `vault_ref`+`fingerprint`+`serial`.
  - **The rotation chain — the Reaper rule `rotate_due_certs`** (the Reaper leader): a scan of active certs `not_after < NOW()+threshold` with a per-cert `hash(cert_id)` jitter **in the predicate** + a ceiling `max_rotations_per_tick` (anti-avalanche on a mass expiry) → CAS `active→rotating` (single-winner) → genkey+CSR → `SignCSR` (Vault PKI) → `WriteKV` (the E3 path convention `secret/<service>/<inc>/tls/<kind>`, pilot — redis; synchronicity with the `rotate_tls` scenario is mandatory — it reads `vault(cert_ref)` by the same convention) → supersede+insert Warrant + spawn **Voyage(`rotate_tls`, kind=scenario, from `archon-system`) in a single PG-tx** → audit. The policy — `keeper.yml::reaper.rules.rotate_due_certs` (`enabled`/`dry_run`/`rotate_threshold`/`rotate_jitter`/`max_rotations_per_tick`); **default OFF + dry_run** (the R1 barrier: auto-replacement of a live TLS — only an explicit opt-in by the operator).
  - **A new keeper-side core module `core.cert.registered`** (E1; base `core.cert`, state `registered`, registration in `coremod` conditional on `CertStore` — the `core.choir` pattern): registers the cert(s) of an incarnation in Warrant, reading the PEM from Vault by `vault_ref` and extracting `serial`/`fingerprint`/`not_after` from the x509 itself (autonomously). Without this step the Reaper is **blind to the primary certs** — the step must be present in create/`rotate_tls` scenarios. Idempotent (the same fingerprint → no-op); output/audit — only non-secret metadata.
  - **Retention** — the Reaper rule `purge_old_certs` (migration 093): a batch deletion of `superseded`/`expired`/`failed` rows older than `max_age` (parity `purge_old_seeds`); `active`/`rotating` are not deleted.
  - **Open (backlog):** a verify phase after rotation (a health-check of the incarnation) and a metric/alert "due-but-not-rotated" (risk R3: the contour is single — the Reaper leader; with the rule disabled the expiry is silent).

- **Amendment (2026-07-09, cert-rotation became config-driven — the source of `scenario`/`pki_role` = the service manifest; `core.cert.issued` mint+enroll).** Refines the rotation chain from the amendment 2026-07-01 under the user decision 2026-07-09: the rotation scenario name and the Vault PKI-role for signing stop being a Reaper hardcode (`rotate_tls` / a fixed role) and are **read from the service manifest** — a new top-level section `certificate_rotation` in `service.yml`. On top of `core.cert.registered` (accounting an already-issued cert) a second state of the same base module is introduced — `core.cert.issued` (**mint+enroll**: Keeper itself issues the cert and registers it in Warrant). The `warrant` schema is NOT changed, no migration; the contract key `rotate_tls` is NOT renamed — only the **source** of its name changes (hardcode → manifest).
  - **(aa) Manifest section `certificate_rotation` (config-driven).** A new top-level section of `service.yml` (spec — [service/manifest.md → Section `certificate`](../service/manifest.md#certificate-section); the section was renamed and reshaped by the 2026-09-03 amendment below, NIM-745): `enable` (bool — turns on service auto-rotation; `false`/no section → off), `scenario` (name of the rotation operational scenario `scenario/<name>/`, conventionally `rotate_tls`), `threshold` (buffer before expiry; **informative for now**, see (dd)), `pki_role` (Vault PKI-role for signing certs of THIS service specifically). The manifest = the **single source** of these four parameters; they are NOT duplicated in step `params`.
  - **(bb) Path B — the Reaper reads `scenario`/`pki_role` from the manifest at rotation time.** When the `rotate_due_certs` rule fires (amendment 2026-07-01) the Reaper, for each due cert, resolves the incarnation's manifest (version = the pinned `incarnation.service_version`, cache ~60s) and takes from its `certificate` section the rotation scenario name (instead of the `rotate_tls` hardcode) and `pki_role` for the keeper-side re-sign. **No `certificate.rotate` block (or `enable: false`) → the service is excluded from rotation WITHOUT falling back to the `rotate_tls` hardcode** (the Reaper skips its due certs). The Warrant schema is left alone: `scenario`/`pki_role` do not land in PG, they are always taken from the git-reviewed manifest of the pinned version.
  - **(cc) Three rotation gates.** A cert is rotated only if all three are true: (1) service-level — `certificate.rotate.enable: true` in the current manifest; (2) per-cert — the `warrant.auto_rotate` flag (the column already exists, migration 092), **default `true`** on issue/registration (this is the "default yes"); (3) operator cluster-level — `keeper.yml::reaper.rules.rotate_due_certs.enabled` (+`dry_run`, default OFF+dry_run, the R1 barrier). **Separation of responsibility:** the *manifest* = "what and with which" (`enable`/`scenario`/`threshold`/`pki_role`), *keeper.yml* = "whether it's on and how cautiously" (`enabled`/`dry_run`/`rotate_jitter`/`max_rotations_per_tick` + `rotate_threshold`).
  - **(dd) threshold is still global.** `manifest.certificate.rotate.threshold` **is parsed and validated**, but the effective scan threshold `not_after < NOW()+threshold` is taken from the global `keeper.yml::reaper.rules.rotate_due_certs.rotate_threshold` (one scan axis per cluster). Per-service `threshold` (+ essence-override) — **follow-up**: the `warrant.rotate_threshold_override` column is reserved for this by migration 092, but the scan does not read it yet.
  - **(ee) `core.cert.issued` — mint+enroll (E1, base `core.cert`, state `issued`).** A second state next to `registered` on the same base module `core.cert` (registration in `coremod` conditional on `CertStore`, the `core.choir`/`core.vault` pattern). Keeper: generates a keypair+CSR → signs it via Vault PKI with role `pki_role` **FROM THE MANIFEST** → writes cert+key to Vault by the E3 path convention `secret/<service>/<incarnation>/tls/<kind>` → registers it in Warrant with `auto_rotate` (param, default `true`). `registered` remains for the case "the cert was issued outside the module" (e.g. the initial `rotate_tls`, having already put the PEM in Vault) — it reads the ready PEM and only registers the metadata (`serial`/`fingerprint`/`not_after` from the x509). Both are idempotent (the same fingerprint → no-op); output/audit — only non-secret metadata. Module spec — [keeper/modules.md → `core.cert`](../keeper/modules.md#corecertregistered--corecertissued).
  - **(ff) PKI-sign from coremod is justified (blast-radius argument).** Although `core.cert.issued` calls Vault PKI from a keeper-side module, the scenario author **cannot** pick an arbitrary PKI role via `params`: the **role** comes from the git-reviewed `service.yml::certificate.pki_role`, the **mount** — from the operator's `keeper.yml::vault.pki_mount` (signature `<pki_mount>/sign/<pki_role>`). Neither is set in a scenario step → a compromised/broken scenario cannot sign a cert with an arbitrary role (unlike passing a role through params). Same invariant at rotation (bb): a single source for the role name — the manifest.
  - **(gg) Rotation — the whole incarnation at once.** Invariant: `rotate_due_certs` rotates a cert **for the whole incarnation at once** (all hosts together, by spawning the rotation scenario on the incarnation), not per-host — symmetric with the fact that a service's server TLS is one per incarnation, not per-host.
  - **(hh) What does NOT change.** The contract key `rotate_tls` (the default scenario name) is not renamed. The R2 exception to the identity invariant (the service cert private key is generated keeper-side and travels Keeper→Vault, only `vault_ref`+`fingerprint`+`serial` land in PG) from amendment 2026-07-01 also applies to `core.cert.issued` — the private key does NOT go outward (into output / to the operator / to PG). Open items (a verify phase after rotation, a "due-but-not-rotated" metric) from amendment 2026-07-01 remain.

- **Amendment (2026-07-24, provision idempotency — a re-run reuses its OWN registry leftovers; NIM-170).** Refines (a) and the self-onboard amendment (2026-07-01): registering a provisioned VM in `souls` is no longer a plain INSERT. Found on a live cloud stand (2026-07-22): `create` of a 3-VM incarnation got past provisioning and died in a later deploy task → `error_locked`; every subsequent `rerun-last` / `scenarios/create` died with `insert soul "<name>-N": soul: SID already exists (constraint souls_pkey): SQLSTATE 23505`, and the only way forward was deleting rows from PG by hand. The scenario-level cause is structural: `destroy`/`unlock` do NOT remove the `souls` rows (`destroy` leaves tombstones by design, forensic > GC, see (z)), so any second provisioning of the same predictable FQDN collided with the first.
  - **(ii) Rollback alone was never sufficient.** The 2026-07-01 orphan-cleanup covers only a failure INSIDE the `core.cloud.created` step. It cannot cover a run that got past provisioning and failed later (nothing rolls back a step that succeeded), nor a re-create after a `destroy`. The registration itself therefore has to be re-runnable; the rollback stays as-is for the in-step case.
  - **(jj) Reuse rule — the row must be this run's own leftover.** `keeper/internal/soul.EnsureProvisionable` replaces `Insert` on this path. A free SID is inserted as before. A taken SID is re-armed for onboarding (`status → pending`, `last_seen_*` cleared) **only if** it (1) carries no live registration — status `pending` (never onboarded) or `destroyed` (cascade tombstone), AND (2) belongs to the current run: a member of this incarnation in `incarnation_membership`, or a member of nothing yet (cloud-create registers souls BEFORE `core.soul.registered` binds them). Anything else — a `connected`/`disconnected`/`revoked`/`expired` host, or a row that belongs only to OTHER incarnations — is refused with `ErrSoulNotProvisionable` naming the status and the owning incarnations, and the row is left untouched. Refusing rather than taking over is the point: a provision run that adopted a foreign row would mint a bootstrap token for a host it does not own. Both phases are single statements (`INSERT … ON CONFLICT DO NOTHING`, then a conditional `UPDATE`), so the ownership predicate is evaluated under the row lock — no read-then-write window. *(Superseded in part by the 2026-07-26 amendment below: `connected`/`disconnected` rows of THIS incarnation are now passed through, not refused.)*
  - **(kk) The token slot is part of the same invariant.** A SID holds at most ONE active bootstrap token (partial unique `bootstrap_tokens_active_by_sid_idx`), so idempotent souls-registration alone just moves the duplicate-key failure to `bootstrap_tokens`. On reuse the module invalidates the previous still-active token (`ExpireActiveBySID`, marker `system-cloud-reprovision`, parity with `system-force-reissue`) before issuing the replacement — the token baked into the userdata of a VM that never came up is void anyway.
  - **(ll) The run's incarnation travels on the module context.** `pluginv1.ApplyRequest` carries `state`+`params` only (only-add, ADR-012) and gaining a run-context field is not warranted for this; `on: keeper` modules execute IN-PROCESS, so the runner attaches the incarnation to the stream context (`coremod/util.WithIncarnation`, read via `IncarnationFrom`). Out-of-process plugins never observe it (context values do not cross gRPC) — keeper-side core only. An absent value means "unknown owner", which makes any bound row non-reusable, never "no owner".
  - **(mm) Boundaries — this is registry idempotency, NOT VM reuse.** A re-run over hosts that ARE alive and onboarded still stops, now with an actionable message instead of a PK error: re-creating a VM behind a live SID is a different decision (reconcile against the cloud, reuse the existing machine) and belongs to the "idempotent reuse of live VMs" work (NIM-16). What this amendment unblocks is the retry after a provisioning that never reached onboarding, and the re-create after a `destroy`. The `created` output gains `reused` (a count of taken-over records, `0` on a clean first run). *(Boundary lifted by the 2026-07-26 amendment: the live-host case is now a converge, not a stop.)*

- **Amendment (2026-07-26, re-create over hosts that are already up converges instead of refusing; NIM-189).** Completes the previous amendment's (mm) boundary. The VM half of it was already done — `CloudDriver.Create` scans the provider for this run's machines, reuses the live ones, gap-fills only what is missing and returns `VmInfo` for the whole roster (NIM-16, tiered to yc/openstack by NIM-27). What remained was keeper-side: the driver handed back a live VM, and `EnsureProvisionable` then refused its SID because the status was `connected`. So a re-`create` over a fully healthy incarnation was still blocked, for exactly the operator flow the previous amendment set out to unblock.
  - **(nn) Decision — provisioning an existing host is a no-op, not an error.** Converge semantics: `create` over hosts that already exist reports "already there" and the run proceeds through its remaining steps, which bring the configuration to the target state. `EnsureProvisionable` therefore gains a third outcome next to `inserted`/`reused`: **`existing`** — the SID carries a `connected`/`disconnected` registration owned by this run (same ownership rule as (jj): a member of this incarnation, or of none yet). The row is **read, never written**: status, `last_seen_*` and the host's identity survive untouched. Re-arming a live host to `pending` would wipe its presence and expose it to the Reaper's pending→`expired` sweep while its stream is still up.
  - **(oo) No token for a host that already holds an identity.** A bootstrap token is a one-time capability; a host that onboarded has a seed and nothing to redeem it against, so the `existing` path issues none and does not touch the SID's token slot either (the onboarding burned the old one). This has a knock-on: in B-flat the delivery step requires a `bootstrap_token` per host and would hard-fail on its absence. `core.cloud.created` therefore marks such entries `onboarded: true` in `hosts[]`, and `core.bootstrap.delivered` **skips** them (counting them in a new `skipped` output) instead of failing. `onboarded: true` is the ONLY thing that excuses a missing token — a tokenless host without the flag is still an error, since silently skipping it would leave the incarnation short of a member with nothing to show for it. In self-onboard, a re-run where every host is up bakes no tokens and the userdata render is skipped altogether (rendering from an empty FQDN→token map is an error by contract, and the reused VMs do not re-run cloud-init anyway).
  - **(pp) The output stays whole — this is a day-2 invariant, not cosmetics.** Passed-through hosts appear in `hosts[]`/`vm_ids` like any other. `covenant.yml` writes both into `incarnation.state` (`provisioned_vm_ids`/`provisioned_sids`) unconditionally when provision is enabled, so a re-run that reported only the hosts it touched would strand the rest at the provider (billing orphans) and lose the SIDs cascade-destroy needs. The `created` output gains `existing` alongside `reused`.
  - **(qq) What still refuses.** `revoked` and `expired` are NOT passed through: `revoked` is a host an operator deliberately cut off at the mTLS level, and adopting it back into a roster would undo that on the quiet; `expired` is a pending record the Reaper timed out — arguably a leftover of the same shape as `pending` and a candidate for reuse, but that is a separate decision, deliberately not taken here. Foreign ownership is unchanged and remains the security boundary: a live host that belongs only to OTHER incarnations is refused exactly as before — passing it through would fold somebody else's machine into this incarnation's roster and deploy onto it.
  - **(rr) Boundary — this converges configuration, it does not repair identity.** If a `souls` row says `connected`/`disconnected` but the VM behind it was deleted out of band, the driver gap-fills a replacement and that replacement gets no token, because the registry still believes the host onboarded; it will not self-onboard. The explicit route stays the recovery: `destroy` (cascade → `destroyed` tombstones) then `create`, which re-arms the rows and mints fresh tokens.

- **Amendment (2026-07-26, a teardown is not done until the provider confirms the VM is gone; NIM-191).** Refines (f) and the `destroyed` cascade of (a): provider delete calls are asynchronous, so a successful `Delete`/`Terminate` RPC means **accepted**, not **deleted**. Every reference driver reported success right after the call. Found while cleaning up after the NIM-173 live runs: two VMs were still running and billed after a teardown that reported OK — a VM deleted while it was still being created stays alive (a provider parks it in `DELETE_FAILED`, a state it never leaves on its own) while the driver said "deleted".
  - **(ss) Why this is worse than a cosmetic mis-report.** The VMs anti-orphan cleanup exists to remove are exactly the ones caught mid-create — i.e. the only ones the silent failure applies to, so the guard failed precisely where it was needed. And Keeper's cascade then ran over them: `souls → destroyed`, active seeds → orphaned, active tokens → burned, for machines that are still up. Registry and cloud diverge silently, and the divergence bills.
  - **(tt) SDK gains `ConfirmDestroy`, joining `Classify`/`Retry`/`WaitUntilReady` in the shared skeleton (f).** The loop is delete → poll until the provider reports the VM missing → **re-issue the delete whenever the probe reports the deletion itself failed** (polling alone cannot rescue a `DELETE_FAILED` VM; nothing else will retry it). It runs on the same `SOUL_CLOUD_WAIT_BUDGET` as the wait phase — tearing down a VM stuck mid-create means waiting out its creation first. The per-provider part is a `GoneProbe` returning a `GoneResult` (`Gone` / terminal `Err` / observed `State` / `DeleteFailed`); the delete/poll/re-issue/ctx-cancel loop and the rendering of the outcome (`ReportDestroy`) are the SDK's, so six drivers do not invent six teardown dialects. Symmetric with `WaitUntilReady`, the diagnosis separates the two failure modes: "the provider kept refusing — these VMs are alive and billed, a larger budget will not help" vs "teardown still in flight — raise the budget".
  - **(uu) The contract changes for drivers: an unconfirmed teardown is `failed=true`, never a success.** A `DestroyEvent` with `failed=true` carries the `vm_id` so the operator can chase the machine. Behaviour change worth flagging: the previous "already absent" event message is now plain `destroyed` — both mean confirmed-gone, and the two were not distinguishable after the delete had been issued anyway.
  - **(vv) Keeper's adapter was the other half of the bug.** `PluginAdapter.Destroy` aggregated `vm_id` from EVERY event, ignoring `failed`, so even a correctly-reported failure would still have fed the cascade. It now collects only confirmed deletions and **fails the whole call** when any VM is unconfirmed; `applyDestroyed` then leaves the registry untouched. Failing conservatively is the right side to err on here — delete is idempotent, so re-running the step is safe, whereas a cascade over live VMs is not undoable.

- **Amendment (2026-08-17, a second source for the driver tuple — the step itself; the Provider/Profile registries stop being mandatory; NIM-668).** The first, additive step of the user's decision 2026-08-09 "**Cloud stops being an entity of its own — a cloud is just a plugin**". Nothing is removed here: the registry path keeps working with unchanged behaviour, it merely stops being the only way in. Removing the entities is NIM-669 (R6).
  - **(ww) This reverses the 2026-06-29 amendment for `profile`, on a different ground.** That amendment removed the inline object as "a vestige of the early design before the Profile registry appeared; no service used it". The ground has since changed: NIM-598 needs three topologies (standalone / sentinel / cluster) × sizes, and a **registry name cannot express a matrix** — every combination needs a row created by hand, and a service repo cannot ship rows. The VM spec was the last service parameter that could not live in `vars/`. So `profile` becomes a typed dispatch: a **string** is a registry row name (the 2026-06-29 path, unchanged, alive until NIM-669), an **object** is the spec itself, handed to `CreateRequest.profile` as-is — Keeper does not look inside it, exactly as it does not look inside what `ResolveProfile` returns today.
  - **(xx) Three more step params replace the three columns of the `providers` row.** `driver` — the CloudDriver plugin alias, taking the place of the `providers.type`→driver resolution. `region` — merged into the credentials map under the same key `Resolve` uses today (`creds[regionKey]`), so the driver sees no change. `fqdn_suffix` — replaces `ResolvedProvider.FQDNSuffix` when predicting `SID = <name>-<i>.<suffix>` for self-onboard "Variant T" (the 2026-07-01 amendment); the user's decision 2026-08-15 was that the suffix and the region move into step params rather than into an entity of their own.
  - **(yy) `credentials` is a step param holding a `vault:` ref — and the name is deliberate.** The render's first phase (`resolveVaultRefs`) already walks all params and replaces any `vault:<mount>/<path>` string with the secret map it reads, so the module needs no resolution logic of its own and **A-flow is preserved**: Keeper reads the secret with its own token, the driver never talks to Vault and gets no `vault_access`. The key name `credentials` (not `credentials_ref`) additionally falls under the `credential` fragment of `sensitiveKeyRe` — masking on every output channel comes for free rather than being a thing to remember. **★ The ref must be written LITERALLY.** Vault-resolve walks the RAW params and runs BEFORE the CEL phase ([ADR-010](0010-templating.md)), so a ref produced by `${ … }` is never resolved: it would reach the module as a plain string. The module refuses that string naming the `vault:` form, instead of passing an unresolved ref to a driver.
  - **(zz) One source or the other, enforced — and across all three states.** `provider` (a row name) **XOR** the inline set. Naming both is a step error raised in `Validate`, not a silent preference of one over the other; so is half an inline pair, and so is `region`/`fqdn_suffix` alongside `provider` (in registry mode those come from the row, and accepting them would create two answers to one question). `destroyed` and `resized` resolve through the same seam as `created` — inline for `created` alone would ship "you can create it but not tear it down".
  - **(aaa) Audit.** `cloud.provisioned` is mandatory (its absence fails the step) and its provider field is a registry name that inline mode does not have. It carries the **driver alias** in that field, plus the alias under `driver` — and, needless to say, never the credentials.
  - **(bbb) Not a new class of access.** A scenario author can already read any Vault path the Keeper token can reach — `${ vault('…') }` and `vault:` refs in `params` are not restricted to an allowlist ([shared/cel/vault.go](../../shared/cel/vault.go) says so explicitly: the path is written by the scenario author, not by the operator). Inline credentials do not widen that perimeter; they only stop requiring a row in a table first.
  - **(ccc) Boundaries.** Validating the profile object against `CloudDriver.Schema()` is **not** done here — it is not done for registry profiles either today (`Schema`/`ValidateProfile` exist in `pluginhost`/SDK with no callers), and it belongs with module-level defaults in NIM-670. Alias defaults, so `credentials`/`region`/`fqdn_suffix` need not be repeated in every step — also NIM-670. This amendment is the explicit path only.

## Amendment 2026-08-19 (NIM-698, [ADR-0083](0083-declared-secret-state-fields.md)): `core.state.present` — the single write of a declared secret

A new keeper-side core module **`core.state.present`** (verb `present`) reads a `state_schema` field declared `type: secret`, mints only the properties that are still missing, writes them to their **derived** Vault paths and returns the **effective** state in its register — *a generator's output is a candidate, a writer's output is the truth*. Present-semantics is what makes a second run keep the first run's password instead of minting one the writer discards while a consumer has already configured the target with it. The register carries **`vault:` references, not plaintext**, so the value never enters `apply_task_register` and never reaches an audit payload; Keeper resolves an own-namespace reference at the register→CEL-root boundary.

`core.vault.kv-read` and `core.vault.kv-present` are unchanged **outside** the service's own namespace and refused **inside** it (`<mount>/<service>/`) — that prefix is derived now, not authored ([ADR-0083](0083-declared-secret-state-fields.md) §7). These two are the pair the load-time scan alone cannot cover: they take their path as a *parameter*, so an authored `path: ${ vars.p }` names no segment to compare and no `vault()` call the evaluated-path guard could see. The keeper dispatcher therefore scans their **rendered** params before `mod.Apply`, where the interpolation is already gone and the value simply is the path; a fenced task fails with the module never invoked.

## Amendment 2026-08-25 (NIM-699, [ADR-0084](0084-explicit-state-capture.md)): `core.state` gains six more states, and the write lands at the step

The module introduced by the previous amendment keeps its base name and grows the rest of the [ADR-057](0057-state-changes-crud-verbs.md) verb set: **`core.state.set`** (which is what `present` did — overwrite the field, resolving declared secrets), plus **`add`** / **`append`** / **`modify`** / **`remove`** / **`unset`**. The address suffix *is* the verb, so a single base module carries seven states, the widest in keeper-side core; the alternative — one module per verb — would repeat the owner-derivation, the schema lookup and the secret resolution seven times over.

**`present` is re-pointed rather than kept.** It now means what it means everywhere else in the tree: write the field only if it has no value yet. The declared-secret rule that made 698's `present` the right name moves *off* the verb — on a property declared `type: secret` **every** verb keeps an existing Vault value and mints only what is missing, `set` included. So `core.state.set` overwrites the field's ordinary content and still does not rotate a live credential; rotation stays inexpressible by design ([ADR-0083](0083-declared-secret-state-fields.md) §4, unchanged).

**The capture point moves.** The field is written to `incarnation.state` **at the step**, inside the run that produced it, instead of at the end-of-run commit `state_changes:` fed. Two consequences an author can rely on: a later task reads what an earlier one wrote, and a run that fails half-way leaves the state it had already captured rather than nothing. Every other keeper-side module is untouched — this amendment adds states to one base, it does not change the dispatcher.

**One flow-control key is refused on the way in.** A `when:` reading `register.*`/`soulprint.*` on **any** `on: keeper` task is now an error (`when_on_keeper_dynamic_unsupported` at parse, `ErrUnsupportedDSL` at render — [ADR-0084](0084-explicit-state-capture.md) F-D). It is the one place this amendment does touch the dispatcher's contract rather than a single base: `when:` is a Soul-side predicate evaluated in the Soul's flow-control sandbox, and a keeper task never reaches a Soul, so the key was accepted and dropped on the floor. That was tolerable while a keeper task only called out to a registry; it is not once a keeper task is the sole writer of `incarnation.state`. A **static** `when:` is untouched — the keeper settles it at render, before the task is routed keeper-side — and the working replacement for the dynamic one is the condition inside the value (`${ cond ? a : b }`, evaluated in the keeper env where a previous keeper task's register is bound). The key-by-key answer for a keeper-side task is [docs/keeper/modules.md](../keeper/modules.md).

## Amendment 2026-09-01 (NIM-748, [ADR-0087](0087-task-side-derived-from-module-address.md)): the dispatcher is the module address, not `on: keeper`

**Not implemented.** Recorded here because the decision is accepted; the code is NIM-749 / NIM-750.

This ADR introduced the keeper-side core with `on: keeper` as its dispatcher.
[ADR-0087](0087-task-side-derived-from-module-address.md) moves the dispatch onto the **module
address**: the keeper-side and Soul-side core registries share no base name, so the address alone
decides the side. `on: keeper` on a keeper-side core address becomes redundant and therefore an
error; the correct form is to omit the key.

Two consequences worth naming here rather than leaving to the implementer. First, the seven
keeper-side bases are `core.bootstrap` / `core.cert` / `core.choir` / `core.cloud` / `core.soul` /
`core.state` / `core.vault`, and **`core.cert` is missing from `shared/coremanifest` today** even
though the states this ADR's 2026-07-01 and 2026-07-09 amendments shipped are live — ADR-0087 rules
that a base absent from that table has **no side** (all new diagnostics stay silent, routing falls
back to the written `on:`), and closes the hole in the same epic. Second, the keeper registry is
**conditional on its dependencies**, so any guard test pinning the table against the registry must
build a full-dependency registry or it passes vacuously on `core.state`, `core.choir`, `core.cert`
and `core.bootstrap`.

Until NIM-749 / NIM-750 land, `on: keeper` remains required and everything above describes the
engine that ships.

★ **All three claims in the paragraph above have since been overtaken (noted under NIM-884; the
text stays as the record of what was decided).** NIM-747/NIM-749 landed, so the side is derived and
`on: keeper` on a core address is refused as redundant. `core.cert` is in the SIDE catalog
(`side.go`) — though still not in `coreModules`, so its params remain unchecked offline, which is
the distinction the paragraph above conflated.
And the seven bases are **not** the seven listed: `core.cloud` left in NIM-761 and `core.ssh`
arrived in NIM-849, so the catalog is `core.bootstrap` / `core.cert` / `core.choir` / `core.soul` /
`core.ssh` / `core.state` / `core.vault`. Read it from `coremanifest.KeeperSideAddrs()`, never from
a list in a document — an address the catalog does not know does not error, it routes Soul-side
(NIM-863).

## Amendment 2026-09-01 (NIM-757): the CloudDriver contract is removed — a cloud driver is an ordinary plugin

★ **Implemented (NIM-761, 2026-09-04).** The contract, the Provider and Profile registries with
their REST and MCP surfaces, the `core.cloud` module, the six `soul-cloud-*` examples, the
`kind: cloud_driver` discriminator and `sdk/clouddriver` are all gone from the tree. Only NIM-762
(web UI, `soul-stack-web`) is outstanding. Written under NIM-759, flipped under NIM-761.

### Why the contract was redundant

Every CloudDriver already **is** a plugin. It is discovered by the same `keeper.yml::plugins`
catalog entry, resolved by the same git resolver, approved by the same Sigil flow, spawned by
the same gRPC-over-stdio handshake and killed by the same SIGTERM. What the separate service
contract added on top of that was a second way to say the same thing: a second `.proto`
service, a second SDK module directory, a second `kind:` value, a second host adapter, a second
declaration shape for the same artifact. None of it bought a capability the SoulModule contract
lacks — `Create` is an Apply, `Destroy` is an Apply, `Resize` is an Apply, and the profile
schema is an input schema.

The user's decision of 2026-09-01: **a cloud driver becomes an ordinary SoulModule plugin
declaring `side: keeper`** in its schema document ([ADR-020](0020-plugin-infrastructure.md)
amendment 2026-09-01, NIM-748). Nothing is added to the platform to make that possible except
the ability to *execute* a keeper-side plugin, which the declaring half already anticipates.

### Credentials reach the plugin as input, and that is what makes the excision complete

Variant A of amendment (d) above is preserved in substance: **the plugin does not talk to
Vault.** Keeper resolves the secret with its own token and hands the plaintext to the plugin.
That much is a coincidence of shape with what ships today.

The consequence matters more than the coincidence. Once the driver is a SoulModule, its
credentials are **ordinary step params**, resolved by the same render machinery that serves any
other module — the `vault:` reference in `params` walked by `resolveVaultRefs` before the CEL
phase, masked on output by the same `sensitiveKeyRe`. So **no cloud-specific credentials channel
remains in keeper at all.** `keeper/internal/coremod/cloud/credentials.go:12-38` —
`ResolvedProvider{Driver, Credentials, FQDNSuffix}`, with `region` folded into the credentials
map under `regionKey` (`:39`) — is exactly the thing that goes away, and with it the
`ProviderResolver`, the Provider→Vault lookup and the `CreateRequest.credentials` /
`DestroyRequest.credentials` fields that carried its output. What remains is a plugin reading a
param, which is not a cloud mechanism.

This is the difference between an excision and a half-done one. If Keeper kept a resolver that
knew what a cloud credential is, the removal would leave the abstraction alive under a different
name; it does not.

### The order cannot be permuted, and the reason is structural

A SoulModule executes **on a host**. A VM is created when **no hosts exist yet** — that is why
`core.cloud` was made keeper-side in the first place (the original Decision above). So the
sequence is forced:

1. **NIM-758 — keeper learns to execute a keeper-side plugin.** This is more than a dispatcher
   patch; there are **three** places that refuse, and only the first is a lookup:
   - `Runner.applyKeeperTask` (`keeper/internal/scenario/keeper_dispatch.go:274-276`) resolves the
     address against `r.keeperModules` — a `coremod.Registry` — and nowhere else; a miss is
     `unknown keeper-side module`.
   - `coremod.Default` (`keeper/internal/coremod/registry.go:200`, the hardcoded map at `:209`)
     builds that registry from in-tree core modules only; a plugin has no way into it.
   - ★ `Host.Spawn` (`keeper/internal/pluginhost/pluginhost.go:117-121`) **refuses** anything that
     is not `cloud_driver` or `ssh_provider` (`pluginhost: expected kind=cloud_driver|ssh_provider,
     got %q`) — even though keeper already discovers and caches `soul_module` plugins, but only to
     hand them to Souls, never to run them itself. The missing half is not discovery, it is
     permission to spawn.

   NIM-749 already landed `side: keeper | soul` in the schema (`sdk/schema/schema.go:147`, with
   `Side`/`SideSoul`/`SideKeeper` at `:102-121`), so the **declaring** half existed and the
   **executing** half did not. ⚠ The hedge in the NIM-748 amendment above ("the code is NIM-749 /
   NIM-750") is stale on its first number: NIM-749 has landed, NIM-750 — `on: keeper` ceasing to be
   required — has not. This gap was tracked as **NIM-758** (epic NIM-757) and, earlier, as
   **NIM-688** — the same gap under two numbers — and **it is closed**: `applyKeeperTask` falls back
   from the core Registry to the discovered plugins, and `Host.SpawnSoulModule` starts one declaring
   `side: keeper`, refusing before the fork any module that declares otherwise.
   ⚠ One thing NIM-758 did **not** move, and step 2 must not assume it did: `on: keeper` stays
   REQUIRED on a plugin address. A plugin's side lives in its stamped schema document, which the
   Keeper reads at dispatch and a scenario cannot read at all — so unlike a core address, a plugin
   address does not announce its own side to the linter or the render pipeline.
2. **NIM-760 — the cloud driver moves to an ordinary plugin with `side: keeper`,** and VM creation
   is verified live against a real provider before anything is deleted.
3. **NIM-761 — removal.** The contract, the registries, the module, the SDK directory, the proto.

Removal first would leave the platform with **no way to create a machine at all** — the old path
deleted and the new path not yet executable. The live verification in step 2 is what makes step 3
a deletion rather than a bet.

### The losses, named — this is an accepted price, not a side effect

**Eight RBAC permissions** leave the catalog (`keeper/internal/rbac/catalog.go:431-436` and
`:460-461`): `provider.create`, `provider.read`, `provider.delete`, `profile.create`,
`profile.read`, `profile.delete`, `provider.label-set`, `profile.label-set`.

**Seven audit events** leave `shared/audit/event_types.go`: `cloud.provisioned` (`:392`, the one
event covering create *and* destroy through a payload `action`), `provider.created` (`:1177`),
`provider.deleted` (`:1182`), `profile.created` (`:1190`), `profile.deleted` (`:1195`),
`provider.label_changed` (`:1295`), `profile.label_changed` (`:1296`). Note that
`shared/audit/event_types_gen.go` is **derived** (`make gen-audit-catalog`) and feeds the OpenAPI
`AuditEvent.type` enum — the catalog edit propagates into the served spec without a hand edit.

**Two Postgres registries** — Provider and Profile: migrations `019_create_providers`,
`020_create_profiles`, `094_add_providers_fqdn_suffix`, and the FK
`profiles.provider → providers(name) ON DELETE RESTRICT` that ties them.

Also going, named so the scope is not underestimated: **10 MCP tools**
(`keeper/internal/mcp/provider.go`, `keeper/internal/mcp/profile.go`); **10 Operator-API
operations over 6 path items** (`/v1/providers*`, `/v1/profiles*`) — the spec is *derived* from
the huma operations, so `docs/keeper/openapi.yaml` follows the Go edit and is never hand-edited;
`shared/coremanifest/mod_cloud.go` **in full**; `keeper/internal/provider/`,
`keeper/internal/profile/`, `keeper/internal/coremod/cloud/`,
`keeper/internal/pluginhost/clouddriver.go`, `sdk/clouddriver/`; the six
`examples/module/soul-cloud-*`; and the Provider / Profile screens in `soul-stack-web`
(**NIM-762**).

**`core.cloud` covers three states — `created` / `destroyed` / `resized` — and all three go.**
`resized` is missing from this ADR's own framing: the original Decision (a) names
`created`/`destroyed`, and `resized` is named only in amendments (u) of 2026-06-26 — in passing,
while correcting the address spelling — and (zz) of 2026-08-17, which does treat it as a first-class
state ("across all three states", "`destroyed` and `resized` resolve through the same seam as
`created`"). Never in the Decision. It is nonetheless a live state with its own RPC
(`CloudDriver.Resize`), its own capability marker (`Resizable`) and its own param set
(`allow_downtime` / `desired`), declared in `shared/coremanifest/mod_cloud.go`. Saying so here is
part of stating the price honestly.

### Removing a permission is a breaking change, and its shape is worse than "a lost grant"

The catalog is a **closed enum**. `ParsePermission`
(`keeper/internal/rbac/parser.go:98-100`) rejects an unknown name with `unknown_permission`.
`NewEnforcerFromSnapshot` (`keeper/internal/rbac/enforcer.go:127-130`) returns on the **first**
unparseable string in the whole snapshot, and `keeper/cmd/keeper/daemon.go:833-837` turns that
error into a start-up refusal. So one surviving `provider.create` row on one obscure role does
not degrade one grant: it prevents the enforcer from being built, which means keeper does not
start, which means the `role.*` API that could delete the row is not serving. That is a
**cluster-wide authorization lockout with no in-band remedy**.

There is a silent window in between. `keeper/internal/rbac/holder.go:265-272` logs a failed
TTL-refresh and keeps serving on the previous enforcer, so a running cluster hides the fault
until its next restart — the failure surfaces at the least convenient moment rather than at the
moment it is introduced.

⚠ Where the grants live: **Postgres (`rbac_role_permissions`), not `keeper.yml`.** The `rbac:`
key was hard-cut by [ADR-028(g)](0028-rbac-storage.md). The doc comments at
`keeper/internal/rbac/catalog.go:4-6` and `keeper/internal/rbac/parser.go:37-38` still say
"loading `keeper.yml` fails"; that is stale prose describing the right failure at the wrong
source. Recorded, not fixed — a Go comment is a `developer` change and out of this ticket's
scope.

**The procedure is settled by precedent, not open.**
`keeper/migrations/109_drop_permission_update_hosts.up.sql` (NIM-330) removed a permission and
documents the rule verbatim. Cite it as the template:

- The **catalog entries and the data migration deleting the rows ship in the same change.** A
  catalog edit without the migration is the lockout above.
- The DELETE matches **both** the bare and the scoped form (`permission IN (…)` **or**
  `permission LIKE 'X on %'`). The ` on ` separator is pinned by `parser.go:48`, so the two
  patterns are exhaustive. Migration 095 matched only the bare form for its own rename and would
  have missed every scoped grant — that is the bug not to repeat.
- A role emptied to **zero** permissions is **kept, not dropped**: it may carry
  `rbac_role_operators` memberships or be a derived role's parent, and dropping it cascades far
  beyond this change. 109 reports such roles with a `RAISE NOTICE` instead.
- `provider.*` / `profile.*` **wildcard** grants need no fix — a wildcard expands over whatever
  the catalog holds at load time, and after this change that expansion is simply shorter.

⚠ Also recorded without fixing: `catalog.go:48-50` states the opposite policy in prose — *"Names
are never removed (operator roles in `keeper.yml` may hold historical names; removal would break
existing installations)."* Its stated **reason** is dead (roles are not in `keeper.yml`); its
**rule** is live for exactly the enum reason above; and NIM-330 already broke it once, with a
migration, deliberately. Flagged so the next reader does not read it as a blocker on this work.

### `proto/plugin` — Option A, no backward compatibility

Decided by the user: `proto/plugin/v1/clouddriver.proto` (`service CloudDriver`, 7 RPCs,
`:17-49`) and its messages are **deleted**, together with the committed generated Go
`proto/plugin/gen/go/v1/clouddriver.pb.go` and `clouddriver_grpc.pb.go`, and the
`sdk/clouddriver/` module directory. The price, stated honestly:

- **An already-built third-party plugin binary does not break.** The wire is untouched; the
  binary keeps serving `soulstack.plugin.v1.CloudDriver` on its socket. It simply stops being
  called, because NIM-761 deletes `keeper/internal/pluginhost/clouddriver.go`. What breaks is the
  **rebuild against a newer tag**: `pluginv1.RegisterCloudDriverServer` and
  `pluginv1.NewCloudDriverClient` become undefined, and `sdk/clouddriver` disappears as a
  separate module breakage.
- ⚠ **`make check-gen` will NOT catch an orphaned generated file.** `Makefile:22` enumerates the
  inputs with `find` over `proto/plugin/v1`, and protoc does not delete stale outputs. Removing the `.proto`
  while leaving the committed `.pb.go` produces an empty `git diff` and a **green** gate. The two
  `.pb.go` files must be deleted **by hand in the same commit** (NIM-761).
- **`KIND_CLOUD_DRIVER = 2`** in `proto/plugin/v1/common.proto:15` goes with the contract, as
  `reserved 2; reserved "KIND_CLOUD_DRIVER";`. **`reserved` here is not backward compatibility** —
  it is the never-reuse rule of [ADR-020(c)](0020-plugin-infrastructure.md), which stays in force
  (precedent: the reserved `PluginSigil` fields). Removing an enum **value** is the one part of
  this that the only-add rule covers literally. ★ An old plugin then fails at **schema-document
  validation** — not at build, and not at handshake. `sdk/schema/validate.go` reaches its
  `default:` arm and emits an error-level `kind_invalid`, and
  `keeper/internal/pluginhost/slot.go:100-102` (`ParseDocument` + `FirstError`) returns
  `pluginhost: invalid schema document in %q` **before the plugin is spawned** — no process starts,
  so nothing is SIGTERMed. The kind-drift check at `shared/pluginhost/handshake.go:84`
  (`ProtoKind` → `KIND_UNSPECIFIED` → drift → SIGTERM) is a real second gate but is unreachable
  here, the slot having been refused first. The distinction matters to whoever words NIM-761's
  error message: the refusal an operator sees is the slot-load one.
- **`proto/plugin/v2` was considered and rejected.** It is the escape hatch
  [ADR-012](0012-keeper-soul-grpc.md) names for breaking changes, but nothing about CloudDriver is
  changing *shape* — it is going away, so there is no second version to carry.
- ⚠ **Correction, because it changes where the work lands.** The closed `kind:` enum that
  [ADR-020(e)](0020-plugin-infrastructure.md) describes lives in **`sdk/schema/schema.go:34-45`**
  (`KindCloudDriver Kind = "cloud_driver"`, `:42`), re-exported at `shared/plugin/document.go:66`
  and validated at `sdk/schema/validate.go:112-119`, `:143-152`, `:192-196` — **not in proto**.
  `pluginv1.Manifest` / `CloudDriverSpec` / `SoulModuleSpec` / `SshProviderSpec` have **zero
  non-test Go references** in this tree: `manifest.proto` is already a hand-synced dead document,
  and ADR-020's "expansion via PR in `proto/plugin/vN/manifest.proto`" (echoed at
  [`docs/naming-rules.md`](../naming-rules.md)) is stale. So *"`kind: cloud_driver` leaves the
  closed enum"* is an **`sdk/` change**, with the proto `reserved` as a footnote.

### NIM-668 is annulled

The amendment of 2026-08-17 above (NIM-668 — the second source for the driver tuple: `driver` /
`credentials` / `region` / `fqdn_suffix` / `profile` as an inline object) is **annulled**. It was
the additive first step of the 2026-08-09 decision "cloud stops being an entity of its own"; the
2026-09-01 decision goes past it. There is no `core.cloud` step left for it to parametrise, so
the second source has nothing to be a second source *of*.

Named by number so a reader does not chase it as live. Its live traces, which go with it:
`shared/coremanifest/mod_cloud.go:5-36`, `docs/keeper/cloud.md` §"Two
sources for the driver: registry or inline", and the ADR-017 index row in
[`docs/adr/README.md`](README.md).

### What no longer ships

**Everything above this amendment is now history, not description.** Gone as of NIM-761: the
`CloudDriver` service contract, the Provider and Profile registries with their REST and MCP
surfaces, the eight permissions, the seven audit events, `core.cloud.created` /
`core.cloud.destroyed` / `core.cloud.resized`, the NIM-668 two-source seam and the six official
`soul-cloud-*` drivers. Migration 119 drops the two tables; `pluginv1.Kind` value `2` and
`PluginManifest.spec` field `8` are `reserved`, so neither number can be reused.

What survives, and deliberately: the retry / wait / confirm-destroy / error-classification
plumbing of `sdk/clouddriver`, which was never about the contract. It moved verbatim to
**`sdk/cloudutil`**, including `SOUL_CLOUD_WAIT_BUDGET` — the env-var name is unchanged because
it is set on deployed Keeper units. Only `ReportDestroy`, which wrote `DestroyEvent`s, died with
the contract.


## Amendment (2026-09-03, `certificate_rotation:` becomes `certificate:` with a nested `rotate:`; NIM-745)

Refines (aa) of the 2026-07-09 amendment. The manifest section it introduced is renamed and
reshaped; nothing about the rotation chain, the three gates, the Warrant schema or the
`rotate_tls` contract key changes.

```yaml
# was                          # is
certificate_rotation:          certificate:
  enable: true                   pki_role: redis-server
  scenario: rotate_tls           rotate:
  threshold: 30d                   enable: true
  pki_role: redis-server           scenario: rotate_tls
                                   threshold: 30d
```

- **`pki_role` moves up a level, and that is the reason for the change.** The role is what a cert
  is **issued** with — `core.cert.issued` mints the first one with it, the Reaper re-signs with the
  same one — so it is not a property of the rotation policy. Under the flat key it was validated
  only when `enable: true`, which made "this service's certs are signed by role X, and no, do not
  auto-rotate them" inexpressible: an author had to switch rotation on to be allowed to name the
  role. `certificate: { pki_role: X }` with **no** `rotate:` block is now a complete section, and
  the nesting is what makes it well-formed rather than a special case. The blast-radius invariant
  of (ff) is untouched — the role still comes from the git-reviewed manifest and never from step
  `params`.
- **The gates read exactly as before.** `certpolicy.Policy.Present` now means "the manifest carries
  a `certificate.rotate:` block", and `Enabled` means that block is on. A section holding only
  `pki_role` resolves to the role with both false, so the Reaper's scan and `core.cert.issued`
  exclude it for the same reason they excluded a service with no section at all. Gate (cc) is
  `certificate.rotate.enable` × per-cert `auto_rotate` × `keeper.yml::reaper.rules.rotate_due_certs.enabled`.
- **No transition window, deliberately.** The old key is refused (`unknown_key`) with a hint
  carrying the new form, not read for a release. Three manifests in the world carried it —
  `examples/service/dragonfly`, `examples/service/redis`, and a downstream redis service, whose section
  was already deleted as an `enable: false` (inert by contract, so identical to absence) — and all
  three are ours. A rotation policy the engine silently ignores is the worse of the two failures,
  which is why the key is refused rather than dropped in silence.
- **`core.cert.issued` still requires rotation to be enabled**, and this amendment does not change
  that. Issuance gated on `pol.Enabled` predates the rename and its fail-fast on an unknown
  rotation scenario exists because an enrolled cert with `auto_rotate: true` would otherwise expire
  in silence. Letting a `pki_role`-only service issue is the natural follow-up and a separate
  decision: it needs the `auto_rotate: false` path thought through first.

Spec — [service/manifest.md → Section `certificate`](../service/manifest.md#certificate-section).
