# Cloud integration (`keeper.cloud`)

Module inside the `keeper` binary, responsible for cloud operations (creating / deleting / polling VMs). Dynamic VM creation is implemented as a **script step with `on: keeper`** via the CloudDriver plugin ([plugins.md](plugins.md)). Service does not know the specifics of clouds - it knows "the step of creating a VM with parameters is needed"; Keeper selects the driver and executes.

## Provider and Profile in Postgres

**Provider** — configured cloud account (AWS account, GCP project, OpenStack-tenant). Stored in Postgres ([storage.md](storage.md)), managed via OpenAPI / MCP. CRUD surface **implemented**:

| Method + path | Permission | MCP-tool | Destination |
|---|---|---|---|
| `POST /v1/providers` | `provider.create` | `keeper.provider.create` | Create Provider; `409 provider-already-exists` for take `name`. |
| `GET /v1/providers` | `provider.read` | `keeper.provider.list` | Enumerate (paged `offset`/`limit`). |
| `GET /v1/providers/{name}` | `provider.read` | `keeper.provider.get` | Read one; `404 not-found`. |
| `DELETE /v1/providers/{name}` | `provider.delete` | `keeper.provider.delete` | Delete; `404 not-found`; `409 provider-has-profiles` with linked Profiles (FK RESTRICT, migration 020). |
| `POST /v1/profiles` | `profile.create` | `keeper.profile.create` | Create Profile; `409 profile-already-exists` per take `name`; `422 validation-failed` to a reference to a non-existent Provider (FK). |
| `GET /v1/profiles` | `profile.read` | `keeper.profile.list` | Enumerate (optional filter `provider=`). |
| `GET /v1/profiles/{name}` | `profile.read` | `keeper.profile.get` | Read one; `404 not-found`. |
| `DELETE /v1/profiles/{name}` | `profile.delete` | `keeper.profile.delete` | Delete; `404 not-found`. |

**Immutability.** `update` operations **no**: Provider/Profile are immutable, change parameters = `delete` + `create`. This is protection against partial mutation `spec` of already living VMs (it is impossible to replace the region/credentials under a running fleet on the fly). Therefore, in the directory [rbac.md](rbac.md#cloud-6--cloudmd) there is only `create`/`read`/`delete` (without `update`), and MCP-tools is `create`/`list`/`get`/`delete`.

**`credentials_ref` — vault path only.** The field accepts the string `vault:<mount>/<path>`; The credentials API themselves **DO NOT resolve and DO NOT return** - returns `credentials_ref` as a path (secret-hygiene, symmetry with jwt-signing-key-ref). The vault secret resolution occurs on the scenario layer when calling `core.cloud.provisioned` (see [Credentials-flow](#credentials-flow)), not in CRUD.

`provider.created` / `provider.deleted` and `profile.created` / `profile.deleted` are written to the audit log ([rbac.md](rbac.md)); read routes do not write audit. The form of Provider/Profile bodies is described below.

```yaml
keeper.provider.create
  name=aws-prod
  type=aws
  region=eu-west-1
  credentials_ref=vault:secret/cloud/aws-prod
```

**Profile** - VM template, reusable. Also in Postgres:

```yaml
keeper.profile.create
  name=redis-medium-eu
  provider=aws-prod
  params={
    image:         ami-0abc123,
    instance_type: t3.medium,
    disk:          { size_gb: 50, type: gp3 },
    network:       { subnet: subnet-xyz, security_groups: [sg-redis] }
  }
  cloud_init=...
```

Profile parameters are validated against `profile_schema`, which driver publishes via RPC `Schema()` ([plugins.md](plugins.md)).

**Default essence in git as a substrate.** The default service parameters are in the `essence/` service repo (see [architecture.md → Essence: build pipeline](../architecture.md)); operator overrides them in `incarnation.spec` via the API. The Provider/Profile themselves are runtime-state, and therefore in Postgres, and not in git ([architecture.md → Soul Stack Artifacts](../architecture.md)).

## Cloud-create as script step

In the cloud-create service script, a normal step with `on: keeper`, using the keeper-side core module `core.cloud.provisioned` ([ADR-017](../adr/0017-keeper-side-core.md), [keeper/modules.md](modules.md)). Previous "core-destiny `cloud-provision`" pattern **repealed by ADR-017**: This is not a task package for Soul, but a keeper-side registry operation, so it is a core module with a dispatcher `on: keeper`, not destiny.

```yaml
- name: provision
  on: keeper                             # step is executed on the keeper
  module: core.cloud.created             # base core.cloud + state created (state is the last address segment)
  params:
    provider: "${ input.spawn.provider }"  # Provider NAME from the providers registry
    profile:  "${ input.spawn.profile }"
    count:    "${ input.spawn.count }"
    userdata: "${ input.spawn.cloud_init }" # opt. cloud-init blob for bootstrap soul
```

> **★ State is the last segment of the `module:` address**, not a separate key. It is written
> `module: core.cloud.created` / `core.cloud.destroyed`; `module forms:
> core.cloud.provisioned` or a separate `state: created` key **does not exist**
> (registry divides address `core.cloud.created` into base `core.cloud` + state `created`,
> there is no `state:` field in the task).

What `core.cloud` (state `created`) does:

1. **Resolve Provider registry.** `params.provider` is the name of the string `providers` (not the name of the CloudDriver plugin). Keeper reads the line, takes `type` (= plugin name `soul-cloud-<type>`), `region` and `credentials_ref`.
2. **Resolves credentials.** By `credentials_ref` (`vault:<mount>/<path>`) Keeper reads the secret from Vault KV with the same keeper-side Vault client as `core.vault.kv-read`, and puts the plain secret + `region` in `CreateRequest.credentials`. The driver is **NOT running in Vault** (see [Credentials-flow](#credentials-flow) below).
3. **Pulls `CloudDriver.Create`** via PluginHost (spawn one-shot, ADR-020): the provider creates a VM, streams progress, waits for readiness (running + IP/DNS) and returns `VmInfo` with `fqdn` (= SID) filled in.
4. For each VM, an entry is created in `souls` with `status: pending` and a bootstrap token is issued under its FQDN (the plain token goes only to register-output - `register.<step>.hosts[i].bootstrap_token`, to the database - hash).
5. Cloud-init on the VM (via `userdata`) puts **setup only**: `soul`-binary, CA, `soul.yml`, systemd-unit - **without token**. The token is delivered by a separate keeper-side step `module: core.bootstrap.delivered` ([ADR-063](../adr/0063-bootstrap-token-delivery.md), [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)); he also makes redeem (`soul init`) on VM. After init, Soul raises EventStream and goes to `connected`.

Steps 4-5 - **B-flat (default)** mode. With `self_onboard: true`, the order is different: tokens are issued **BEFORE** create and baked in userdata - VM onboards itself, the delivery step is not needed (see Self-onboard "Option T").

State `destroyed` is symmetrical: Keeper resolves the same Provider (for credentials) and calls `CloudDriver.Destroy(vm_ids, credentials)`, then the cascade transaction of registries ([ADR-017](../adr/0017-keeper-side-core.md)).

### Credentials-flow

Option **A** (fixed): **Keeper resolves the secret from Vault and passes plain to the driver; the driver does not go to Vault.**

- The secret is stored in Vault, in the registry `providers` - only `credentials_ref` (`vault:<mount>/<path>`).
- For each `Create`/`Destroy` Keeper reads the secret (keeper-side Vault client), adds its fields + `region` to `CreateRequest.credentials` / `DestroyRequest.credentials` (`google.protobuf.Struct`, only-add fields to [`clouddriver.proto`](plugins.md)).
- `region` lives **inside** the credentials/profile Struct, and not as a separate typed field: it is provider-specific (Proxmox/OpenStack does not have its own `region`).
- The secret is **masked on any output** (audit / OTel / SSE / error messages) by the same `MaskSecrets` that cleans the bootstrap token and vault refs. Drivers therefore **do not need** capability `vault_access`.

> **Cloud-init bootstrap (B-flat, ADR-017(h) amendment 2026-05-27).** Implemented MVP: per-VM bootstrap token is issued AFTER `VmInfo` is returned (when the SID is known) and placed in `register.<step>.hosts[i].bootstrap_token`. **Cloud-init userdata does NOT carry tokens** (cloud-provider API stores userdata in plaintext metadata, accessible to VM processes) - contains only: installation of `soul` binary (curl with pinned-CA), embedded PEM CA Keeper (`/etc/soul/tls/keeper-ca.pem`), minimal `soul.yml` with `keeper.endpoints`, systemd-unit `soul.service`. **Delivery of per-VM token to VM is a separate keeper-side script step `module: core.bootstrap.delivered`** ([ADR-063](../adr/0063-bootstrap-token-delivery.md); previous stub `keeper.push.applied` rejected - such keeper-side module did not exist, BUG#2): step reads `${ register.<step>.hosts }` (`sid`/`primary_ip`/`bootstrap_token`), via SSH puts a token on the VM (STDIN, not argv) and makes a redeem there - `soul init`. See ["Cloud-init bootstrap (MVP)"](#cloud-init-bootstrap-mvp) below and [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered).

> **Coven binding of a new host is a separate scenario step.** `core.cloud.provisioned` creates VMs and registers them in `souls`, but **does not assign coven labels by itself**: writing `souls → coven` is a separate scenario step through the core module `core.soul.registered` ([`docs/keeper/modules.md`](modules.md)). The separation is deliberate: cloud-create and binding to coven are different registry operations, each with its own guard-rails, and they are assembled independently in the script.

The next steps of the script follow the coven of this incarnation (`on: ["{{ incarnation.name }}"]`) and set the destiny to the newly created hosts.

## Security destroy

Removing a VM is a destructive operation, guard-rails are required:

- **Tombstone period.** With scale-down or `incarnation.destroy`, the VM is not deleted immediately - it is marked with `marked_for_deletion`, waits for `tombstone_ttl` (default 24h). The operator can roll back.
- **Confirm flag.** Reconcile/destroy does not do physical destroy without an explicit `--allow-destroy` or corresponding field.
- **Storage protection.** EBS-volumes / disks are not deleted along with the VM by default - they are confirmed separately.
- **Audit.** Each deletion is written to the log indicating the initiator ([rbac.md](rbac.md)).

This is critical - otherwise one typo in `count` will erase the rest.

## Cloud-init bootstrap (MVP)

Implemented B-flat option (ADR-017(h) amendment 2026-05-27). Goal: a new VM created by `core.cloud.provisioned` raises the `soul` agent and connects to the Keeper cluster.

Bootstrap token delivery - three modes:

1. **B-flat (default)** — userdata carries only setup, **without tokens**; the token is delivered by a separate step `core.bootstrap.delivered` (token-only). Described in [Flow](#flow) below.
2. **Full-install** - platforms without cloud-init userdata: `core.bootstrap.delivered` with `install: true` (`transport: teleport`) itself installs the entire setup via SSH and delivers the token ([ADR-063](../adr/0063-bootstrap-token-delivery.md), [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)).
3. **Self-onboard "Option T"** - `self_onboard: true` on `core.cloud.created`: per-VM tokens are baked in userdata, VM onboards **itself in one cloud-init cycle**, there is no delivery step ([ADR-017(h) amendment 2026-07-01](../adr/0017-keeper-side-core.md)). See subsection below.

### Flow

1. **Script step `module: core.cloud.created`** with parameter `generate_userdata: true`:
   - Keeper resolves `keeper.yml::cloud_init.tls_ca_ref` to PEM CA via Vault (field `ca`).
   - Keeper renders cloud-config YAML using embed template `keeper/internal/soulinstall/templates/cloud-init.tmpl`. Install-blueprint (paths/permissions/soul.yml/unit) is included in the shared package [`keeper/internal/soulinstall`](../../keeper/internal/soulinstall) - a single source for the userdata path **and** full-install via SSH ([ADR-063](../adr/0063-bootstrap-token-delivery.md) amendment 2026-06-30); `keeper/internal/cloudinit` remains a config resolver (Vault) and a thin render wrapper.
   - Userdata goes to `CreateRequest.userdata` (ADR-017(e) only-add), the provider creates a VM with this userdata.
   - After Create - Keeper issues a per-VM bootstrap token under the VM FQDN, puts it in `register.<step>.hosts[i].bootstrap_token` (plaintext, masked on all audit/SSE/OTel outputs by the `audit.MaskSecrets` substring filter).
2. **Cloud-init is running on the VM:**
   - Sets CA: `/etc/soul/tls/keeper-ca.pem` (PEM, embedded in userdata).
   - Downloads the `soul` binary: `curl --cacert /etc/soul/tls/keeper-ca.pem $SOUL_BINARY_URL` → `/usr/local/bin/soul` (pinned-CA, TOFU-mitigation). With `soul_binary_ca: system` curl goes without `--cacert` (system trust-store, for public CA artifact hosts); see the field in the "Config" section.
   - Writes the minimum `/etc/soul/soul.yml` from `keeper.endpoints[0] = {host, bootstrap_port, event_stream_port}` (`event_stream_port` - from `cloud_init.event_stream_port`; not specified → fallback to port `bootstrap_endpoint`, single-port LB).
   - `systemctl daemon-reload + enable + start soul.service`. Without SoulSeed, the demon **does not board itself**: `soul run` ends with the error "SoulSeed not found - run `soul init` first" and is restarted by systemd until the token is delivered and redeemed (step 3).
3. **The next step of the script is token delivery: `module: core.bootstrap.delivered`** ([ADR-063](../adr/0063-bootstrap-token-delivery.md), specification - [modules.md → core.bootstrap.delivered](modules.md#corebootstrapdelivered)):
   - Reads `${ register.<step1>.hosts }` (`sid` / `primary_ip` / `bootstrap_token`) via CEL.
   - Via SSH (direct mode via SshProvider plugin + CA-signed host-cert, or `transport: teleport` by-name) puts the token in `token_path` (default `/etc/soul/token`); **token goes to STDIN, not to argv**.
   - Ibid **redeem token**: `test -e <seed-cert> || SOUL_BOOTSTRAP_TOKEN="$(cat <token_path>)" soul init --config /etc/soul/soul.yml` - idempotent (guard by seed-cert; single-use token). With `start_soul: true` (default) - `systemctl daemon-reload && enable && start soul`.
4. **`soul init` → Bootstrap-RPC** (ADR-012(b)): CSR with token → Vault PKI signature → mTLS-cert (SoulSeed). Next, the demon holds the EventStream; VM set onboarding waits for barrier `await_online` step `core.soul.registered` ([ADR-061](../adr/0061-onboarding-await-and-midrun-reresolve.md)).

### Config

Block `keeper.yml::cloud_init` (optional):

```yaml
cloud_init:
  bootstrap_endpoint: lb.keeper.example:9442      # LB host:port (Bootstrap-RPC listener)
  event_stream_port:  9443                         # opt: EventStream TCP port (mTLS); 0/no → bootstrap_endpoint port
  tls_ca_ref:         vault:secret/keeper/ca      # PEM CA, field `ca` in KV
  soul_binary_url:    https://artifacts.example/soul/v1.0.0/soul
  soul_binary_ca:     keeper                       # opt: keeper (default) | system
  soul_version:       v1.0.0                       # opt. diagnostic label
```

The same block is a single source of install parameters for **full-install mode** `core.bootstrap.delivered` (platforms without cloud-init userdata): the name `cloud_init` was saved deliberately, the second block with the same content would be drift ([ADR-063](../adr/0063-bootstrap-token-delivery.md) amendment 2026-06-30).

Fields:

- `bootstrap_endpoint` — `host:port` LB (Bootstrap-RPC listener).
- `event_stream_port` - opt. TCP port of the EventStream phase (mTLS) of the same host; falls into `event_stream_port` of the generated `soul.yml`. `0`/omitted → back-compat fallback to port `bootstrap_endpoint` (single-port LB). Without it, on topologies with separate ports `soul run` would have called EventStream on the Bootstrap-only listener ("Unimplemented: method EventStream", 6th wall [ADR-063](../adr/0063-bootstrap-token-delivery.md)).
- `tls_ca_ref` — vault-ref (`vault:<mount>/<path>`) on PEM-CA Keeper (field `ca` in KV).
- `soul_binary_url` — HTTPS URL for downloading the `soul` binary (plain http is rejected).
- `soul_binary_ca` — which trust-store curl uses when downloading the binary:
  - `keeper` (default, empty value) - pin on keeper-CA (`curl --cacert keeper-ca.pem`); for a self-hosted artifact host with the same CA as Keeper;
  - `system` - system trust-store (`curl` without `--cacert`); for artifact hosts with a public CA (for example, the binary on Nexus for GlobalSign).
  - `soul_binary_ca: system` weakens **only** artifact host certificate verification when curling a binary. The Bootstrap channel (souls↔keeper mTLS) is pinned to the keeper-CA **always**, regardless of this field; `system` is still system-CA-over-TLS, not plain-http.
- `soul_version` - opt. diagnostic label.

Hot-reload works: editing `keeper.yml` via `keeper-reload` → the next cloud-create step renders userdata with a new snapshot (without restarting Keeper).

If there is no block, the `generate_userdata: true` parameter fails the script step with an understandable error; explicit `userdata: "<blob>"` continues to run (legacy/gold-image flow).

### scenario parameter

```yaml
- name: provision
  on: keeper
  module: core.cloud.created           # base core.cloud + state created (NOT core.cloud.provisioned; state is the last segment)
  params:
    provider:          aws-prod
    profile:           redis-medium-eu # registry entry NAME profiles (NOT inline-object - ADR-017 amendment 2026-06-29)
    count:             3
    generate_userdata: true            # ← render from keeper.yml::cloud_init
  register: vm
```

`profile` - **name** of the registry line `profiles` (`POST /v1/profiles` before running): Keeper resolves the name in VM-spec params through the Profile registry, symmetrically `provider`→credentials. Inline-object in `params.profile` **not supported** (a vestige of an early design before the advent of the Profile registry; [ADR-017](../adr/0017-keeper-side-core.md) amendment 2026-06-29).

`generate_userdata: true` and `userdata: "..."` - **mutually exclusive** (simultaneous presence → fail). Without both, the provider receives an empty userdata. `self_onboard: true` is also mutually exclusive with the explicit `userdata:` - the keeper must bake the tokens himself (see below).

### Self-onboard "Option T"

Third mode of bootstrap delivery ([ADR-017(h) amendment 2026-07-01](../adr/0017-keeper-side-core.md)): VM onboards **itself in one cloud-init cycle**, without the `core.bootstrap.delivered` step and without claim-callback. Chicken-egg "SID is known only AFTER create" removed by FQDN prediction: keeper itself sets the base name of the VM batch (param `name` → `CreateRequest.name`, the driver names the VM `<name>-<index>`) and knows the FQDN suffix of the provider (registry field `providers.fqdn_suffix`, migration 094) - full FQDN `<name>-<index>.<fqdn_suffix>` is known to each VM BEFORE create.

```yaml
- name: provision
  on: keeper
  module: core.cloud.created
  params:
    provider:     dev-cloud       # Provider must have fqdn_suffix set
    profile:      redis-medium-eu
    count:        3
    name:         soul-e2e        # base-name: FQDN = soul-e2e-<i>.<fqdn_suffix>
    self_onboard: true            # generate_userdata is implied
  register: vm
```

How it works:

1. Keeper **BEFORE create** writes per-VM bootstrap tokens to the predicted SIDs (records `souls` with `status: pending`, in `bootstrap_tokens` - hash) and renders userdata with map FQDN→token: file `/etc/soul/self-onboard-tokens` (`0600`, lines `<fqdn> <token>`).
2. The provider creates a VM with this userdata; keeper checks the actual FQDNs with the predicted ones - discrepancy (the driver did not take into account `CreateRequest.name`) → fail-fast, otherwise the token would not match the hostname of the VM. Failure of create/reconciliation **rolls back** inserted souls/tokens (orphan-cleanup - without it, rerun would run into a PK conflict).
3. cloud-init on the VM installs the usual setup (CA, `soul.yml`, unit, `soul`-binary), then **between installing the binary and starting `soul.service`** selects its token line by `$(hostname -f)` and does `soul init` (the token goes through env `SOUL_BOOTSTRAP_TOKEN`, not argv). After init `soul.service` raises the already boarded demon; There is a standard barrier `await_online` (`core.soul.registered`) waiting for recruitment onboarding.

Contract params: `self_onboard: true` (bool, opt) **requires `name`**; **mutually exclusive with explicit `userdata:`**; `generate_userdata` is implied - block `keeper.yml::cloud_init` must be configured. **Plain token is NOT placed in register-output** - there is no key `bootstrap_token` in `register.<step>.hosts[i]` in this mode (no delivery); output carries the flag `self_onboard: true`.

> **★ Security is a deliberate departure from B-flat.** B-flat keeps userdata "without tokens" (cloud-provider stores userdata in plaintext-metadata, accessible to VM processes). Self-onboard puts tokens in userdata deliberately: they are **single-use** - redeem occurs immediately, on the first boot cycle, re-use is impossible; the alternative is mandatory push delivery, which is not available on some platforms. Mode - **opt-in per-step** (`self_onboard`); default remains B-flat.

Bounds: provider without predictable FQDN (empty `fqdn_suffix`) - clear step error; on platforms with userdata disabled (WB `ci_user_data`, [ADR-066](../adr/0066-teleport-onboarding-profile.md)), the mode is not available - there is a standard full-install path via Teleport ([ADR-063](../adr/0063-bootstrap-token-delivery.md)).

### Security

- **Userdata does NOT carry tokens** (cloud-provider plaintext metadata) - in default B-flat mode. The exception is opt-in self-onboard "Option T": tokens in userdata are deliberate, single-use.
- **Pinned-CA for curl** (`soul_binary_ca: keeper`, default) - an attacker cannot replace the `soul` binary with a man-in-the-middle (requires ownership of the CA Keeper privateer). With `soul_binary_ca: system`, the cert artifact host is verified using the system trust-store (for public CAs); **only** this step is weakened - the Bootstrap channel (souls↔keeper mTLS) is always pinned to keeper-CA, plain-http is still rejected.
- **TLS CA from Vault** - single source of truth, rotation without keeper.yml edits.
- **Per-VM token is delivered in a separate step via SSH** (`core.bootstrap.delivered`: token in STDIN, not in argv; audit-payload without tokens) - the attack surface is limited to SSH access, not cloud metadata.

### Example

See [`examples/service/example-cloud-bootstrap/`](../../examples/service/example-cloud-bootstrap/) - complete scenario create with `core.cloud.provisioned` + per-VM-token.

## List of cloud providers for MVP

AWS / GCP / Azure / Yandex Cloud / OpenStack / vSphere / Proxmox - which of them is supplied in the first release, and which is the extension community: [open Q No. 13](../architecture.md).

Reconcile-loop "declared count vs actual VM count" (background alignment) - [open Q No. 17](../architecture.md): lay down the MVP immediately or only manual `incarnation.upgrade/scale`.

## See also

- [plugins.md](plugins.md) - contract `CloudDriver`.
- [storage.md](storage.md) - where Provider, Profile, VM registry live.
- [rbac.md](rbac.md) - RBAC for cloud operations.
- [config.md](config.md) → `plugins.cloud_drivers` - driver registry.
- [architecture.md → Cloud integration via `keeper.cloud`](../architecture.md).
- [architecture.md → Targeting and host communication](../architecture.md) — `on: keeper` vs `on: [coven, …]`.
- [naming-rules.md](../naming-rules.md) — `keeper.cloud`, `CloudDriver`, Provider, Profile.
