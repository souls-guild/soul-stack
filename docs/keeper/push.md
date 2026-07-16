# Push mode (`keeper.push`)

A module inside the `keeper` binary for managing hosts **without installing a Soul agent**. Keeper goes to the host over SSH, executes the Destiny steps, collects the results; nothing runs on the host between runs.

Not a separate binary ([ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)) — this is a server function (RBAC, audit, issuing SSH credentials via Vault), its place is in Keeper, not in a client binary.

## Purpose

Used for:

- one-off tasks where a permanent agent is excessive;
- hosts where the security policy forbids a long-lived daemon with privileges;
- initial automation before a decision is made to install a Soul agent;
- bootstrap scenarios: roll out the `soul` binary and a SoulSeed token in push mode, then work in pull.

## Model

- **A single registry.** A push host is a record in the same `souls` table with `transport: ssh`. The `last_seen_at`, `last_seen_by_kid`, `coven`, `registered_at` columns make sense here too (`last_seen_at` is updated by the fact of the last push run, not a stream). See [storage.md](storage.md) and [`../soul/identity.md`](../soul/identity.md).
- **`soul_seeds` is not used for push.** Push hosts have no mTLS identity — no certificate, no private key, nothing to rotate.
- **No daemon.** Between runs the host does nothing; no stream hangs.
- **Audit.** Each push run is an event in Keeper's journal (what, where, by whom, result) with an RBAC filter ([rbac.md](rbac.md)).
- **push ↔ agent migration.** A host can be in `transport: ssh` and then migrated to `transport: agent` (a Soul was installed) — the record is the same, the field changes, history is not lost.

## SSH authentication — pluggable provider

The specific implementation is **not fixed** ([open Q SSH-2 / #3](../architecture.md)). The principle: SSH authentication goes through a pluggable provider interface; in the first release at least one reference implementation is shipped, the rest plug in through the same interface. Candidates:

- **Vault SSH CA.** On each push Keeper requests a short-lived SSH certificate from Vault and uses it. The hosts trust the Vault CA. Without long-lived keys. The best option security-wise, aligned with the already-mandatory Vault.
- **Static key.** A long-lived SSH key on the keeper host, its public part in the target hosts' `authorized_keys`. For dev/test and installations without Vault.
- **Teleport.** Integration with a Teleport bastion: Keeper goes through the Teleport proxy, using Teleport-issued SSH certificates.

All three fit under the single **`SshProvider`** contract (a gRPC-stdio plugin: `Sign` / `Authorize`). Contract details — in [plugins.md](plugins.md). The provider catalog in the config — [config.md](config.md) → `plugins.ssh_providers`.

## Operator interface

Push, like all of Keeper, is exposed via **OpenAPI and MCP** ([ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)). A CLI wrapper is allowed as a thin utility over the API, not the primary interface. A typical flow:

1. The operator prepares an inventory (a list of hosts and/or Coven labels) and Destiny.
2. Calls `POST /v1/push/apply` (or the MCP tool `keeper.push.apply`) with the inventory, a reference to Destiny, and options. The normative request/response specification — [operator-api/push.md → `POST /v1/push/apply`](operator-api/push.md#post-v1pushapply--push-run-of-destiny-over-ssh).
3. For each host, Keeper: obtains SSH credentials from the chosen provider, opens an SSH session, executes the Destiny steps, collects the result.
4. The run journal is available through the same API/MCP.

## Delivery of the `soul` binary and modules to the host

Push mode reuses the same `soul` binary and the same modules as pull ([architecture.md → Module model](../architecture.md), [`../soul/modules.md`](../soul/modules.md)). The binary runs in one-shot mode `soul apply`: it receives a rendered `ApplyRequest` (protojson) via stdin, applies it, and returns an NDJSON stream of `TaskEvent` + a final `RunResult` (protojson) on stdout, exiting with exit 0 when `RunResult.status==success` and 1 otherwise. The proto contract is common with pull (ADR-012), no push-only schema needs to be introduced.

### Layout on the host

```
/var/lib/soul-stack/
  bin/
    soul-<sha>          # the current version + 1–2 previous ones for rollback
  modules/
    soul-mod-<name>-<sha>
    ...
```

### Algorithm of each push run

1. Keeper connects to the host through the chosen SSH provider.
2. Compares by SHA-256 the target version of the `soul` binary with what lies in `/var/lib/soul-stack/bin/`. Matches — copying is skipped, otherwise the binary is delivered.
3. **All modules registered in Keeper** are transferred (without static analysis of the Destiny). Comparison by SHA-256 per module; nothing changed — copying is skipped. Works thanks to the hot cache.
4. `soul apply` is launched — the rendered plan (`ApplyRequest`: `apply_id` + `RenderedTask[]` after Keeper-side phases `vault-resolve → input-validation → CEL-render → text/template-render`, ADR-012(d)) is passed to stdin as protojson and is not written to disk. Raw Destiny/Essence does not reach the push host — Keeper resolves Vault on its side, the Soul only executes the plan. Stdout is read as an NDJSON stream of `TaskEvent` + a final `RunResult`.
5. Afterwards — the artifacts remain in the cache; host-side cleanup of stale versions is a separate operation, see [`../soul/modules.md`](../soul/modules.md).

### Key properties

- **We transfer everything, we do not guess.** Static analysis of "which modules this particular Destiny needs" is deceptive (modules are invoked dynamically from templates and conditions). Transferring all + a hash cache = simple and without surprises. A future optimization — mandatory declaration of `required_modules` in Destiny — is described in [open Q No. 5](../architecture.md).
- **Hash cache.** The first run on a new host is slow (the binary and all modules are copied). Subsequent ones are instant.
- **Versions are stored explicitly.** The file name contains the SHA, which allows keeping several versions side by side and rolling back without re-downloading.
- **The module registry is in Keeper** - where it is physically (Postgres `bytea` / a separate artifact store / the FS) - [open Q No. 5](../architecture.md). Affects operations and backup strategy, but not the delivery protocol itself.

## Cleanup on the host

Keeper's Reaper does **not** go to hosts — it works only over Postgres ([reaper.md](reaper.md)). Removal of stale files in `/var/lib/soul-stack/` is arranged differently:

- In push mode, cleanup can go within `keeper.push` itself (optionally, by a policy flag): comparison of the local cache with the module registry and removal of stale versions in the same SSH session.
- On revoke (`revoke`) or removal of a host from the registry, the operator can initiate `keeper.push.cleanup` — a separate push operation that wipes `/var/lib/soul-stack/` entirely on the specified host.

Details and the pull variant of cleanup — in [`../soul/modules.md`](../soul/modules.md).

## Wire-up of SshDispatcher in the daemon (S6 pilot, 2026-05-26)

The pilot phase of enabling `keeper.push.apply` in prod (single-CA, config-backed targets/providers, single-provider routing). The long-term canon (S7) — migration to `souls.ssh_target jsonb` + the PG table `push_providers` + `push.host_ca_refs[]` — is a separate slice; the pilot will be deprecated right after S7.

`setupPushDispatchers` (`keeper/cmd/keeper/daemon.go`) is brought up right after `setupPushOrchestrator` and before `setupGRPCEventStream`:

1. **Gate checks.** An empty `plugins.ssh_providers[]` OR no discovered SshProvider plugins in the cache OR a missing `push.host_ca_ref` → push is disabled (WARN in the log, `api.Deps.PushRun=nil`, `/v1/push/*` and `keeper.push.apply` return "not configured"). This is the normal mode of a pull-only installation — without a start error.

2. **Host-CA resolution.** `push.host_ca_refs[]` (S7-3 multi-CA) → `push.LoadHostCAs` for each ref reads Vault KV, parses the PEM `public_key` into an `ssh.PublicKey`, and assembles `[]NamedHostKeyAuthority` (the name from the operator's `name`). Backward-compat: with an empty `host_ca_refs[]` and a filled singular `host_ca_ref` the daemon auto-adapts the singular into a singleton set with the auto-name `default` + a one-time WARN. Any resolution error (Vault unavailable / field absent / broken PEM) is a **fail-fast**: keeper refuses to start (`errSetupFailed`) with the name of the failing CA in the message. The operator explicitly declared push in the config, silently disabling it without a host-CA is not allowed.

3. **Spawn the SshProvider plugin (single-provider pilot).** The **first** discovered SshProvider plugin is taken (in the order of `pluginhost.Discover`/`FilterByCatalog`). Multi-provider routing (`push_runs.ssh_provider` → adapter choice) is deferred to S7.

4. **Env-payload params.** For a plugin named `<name>`, the record `push.providers[].name == <name>` is looked up. If present and `params` is non-empty — it is serialized to JSON and put into the plugin's env variable `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` ([ADR-020 amendment l](../adr/0020-plugin-infrastructure.md)). Example: `vault-bastion` → `SOUL_SSH_VAULT_BASTION_PARAMS`. Record absent / params empty → the plugin starts without an env-payload.

5. **Assembly of SshDispatcher.** `push.NewSshDispatcher` with:
   - `Provider` — a wrapper `pluginhost.SshProviderPlugin` over the spawned plugin;
   - `Targets` — `push.NewConfigTargetResolver(cfg.Push.Targets)` (per-SID lookup, defaults port=22/user=root/soul_path=/usr/local/bin/soul);
   - `Souls` — `push.NewPGSoulLookup(pool)` (checking the precondition `transport=ssh`);
   - `HostAuthorities` — the resolved multi-CA set (S7-3, OR-check via `ssh.CertChecker.IsHostAuthority`);
   - `Metrics` — `push.Metrics` (the counter `keeper_push_host_ca_used_total{ca_name=...}` on each CA match);
   - `Deliverer` / `Cleaner` — `ShaDeliverer` / `ShaCleaner` (S1/S5).

6. **Plugin-handle lifecycle.** The spawned plugin is held **until shutdown** (a long-living handle, unlike cloud plugins where Spawn is per-RPC). Close is registered in the `cleanups` stack AFTER all ssh consumers (LIFO will run it BEFORE Redis/Pool — the plugin holds a unix socket on the keeper-host side, reasonable to tidy up first).

7. **`finalizePushOrchestrator` assembles `*pushorch.PushRun`.** If `d.pushDispatcher != nil` after `setupPushDispatchers`, then after `setupGRPCEventStream` (where the `topologyResolver` is created) a `pushorch.PushRun` is assembled with all deps; otherwise it stays nil (`api.Deps.PushRun=nil`).

### When push is **enabled**

- `plugins.ssh_providers[]` is non-empty AND at least one plugin is discovered in the cache;
- `push.host_ca_refs[]` (S7-3 canonical) is non-empty OR the singular `push.host_ca_ref` (deprecated, auto-adapt) is set; in any case it resolves from Vault, the `public_key` field is a valid PEM SSH public key;
- at least one `souls.ssh_target jsonb` record (S7-1, canonical) or a non-empty `push.targets[]` + `push.allow_legacy_push_targets: true` (legacy fallback under a deprecation window).

## S7-1 migration to souls.ssh_target (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-1) moves the per-host SSH details of the push flow out of `keeper.yml::push.targets[]` (pilot S6) into the `souls` registry: a new column `souls.ssh_target jsonb` with a shape-CHECK `{ssh_port:int, ssh_user:text, soul_path:text}` becomes the canonical source.

**Write path:** `PUT /v1/souls/{sid}/ssh-target` (permission `soul.ssh-target-update`, audit `soul.ssh-target.updated`) or the MCP tool `keeper.soul.ssh-target.update`. soulctl: `soulctl souls ssh-target set <sid> --port … --user … --soul-path …`.

**Read path (the resolver):** `PGFallbackTargetResolver` (`keeper/internal/push/target_pg.go`) on every `SshDispatcher.SendApply` does a SELECT by the PK `souls.sid` and:

- PG-row.ssh_target is set → returns an SSHTarget with defaults substituted (port 22 / user root / soul-path `/usr/local/bin/soul`) for the omitted fields.
- PG-row.ssh_target IS NULL and `push.allow_legacy_push_targets: false` (default) → `ErrTargetNotConfigured` (fail-closed, the operator sees a clear message in `push_runs.summary`).
- PG-row.ssh_target IS NULL and `push.allow_legacy_push_targets: true` → a one-time WARN deprecation + fallback to `ConfigTargetResolver` over `keeper.yml::push.targets[]`.

**Deprecation policy (PM-decision):** 1 release WARN → hard-cut. After S7 closes, the inline form `push.targets[]` is rejected at schema validation as `unknown_key`.

**What is NOT in S7-1:**
- Auto-import of keeper.yml::push.targets[] into `souls.ssh_target` — deferred to S7-4 (requires explicit consent + idempotency guarantees so as not to overwrite operator-hand-set values).
- The `push_providers` PG table (the env-payload of SSH plugins) — S7-2.

## S7-2 migration to push_providers PG-table (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-2) moves the per-provider env-payload params of SSH plugins out of `keeper.yml::push.providers[]` (pilot S6 / S7-1) into the PG table `push_providers` (migration 054). The entity is implemented as an "SSH Provider" variant of Provider (PM-decision: an extension of the existing Provider entity, not a new entity): the same provider concept, but different tables (`providers` for cloud, `push_providers` for ssh), different params schemas and different RBAC permission areas (`provider.*` vs `push-provider.*`).

**Name/format.**

- PK: `name TEXT`, the regex `^[a-z][a-z0-9-]{0,62}$` (env-var-name-safe — the name is translated into `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS`, a leading digit/hyphen would break the env name; one constraint stricter than a cloud Provider).
- `params JSONB NOT NULL DEFAULT '{}'` — the opaque form of the plugin itself.
- `created_by_aid TEXT NOT NULL REFERENCES operators(aid)` — changes only through Archons.
- `updated_at` / `updated_by_aid` — for triage audit (last-write-wins, no version vector).

**Sensitive params (PM-decision S7-2 #5).** Real secrets (`secret_id` / `token` / `password` / `private_key`) MUST be vault-refs (`vault:<path>`); a plaintext value under these keys is rejected at the service layer (`pushprovider.Service.validateSensitive`) with `ErrSensitiveNotVaultRef` (422 validation-failed). The key allow-list is opaque, extension via a PR in `keeper/internal/pushprovider/service.go::sensitiveKeys`. Non-sensitive params (`vault_addr` / `role` / `proxy_addr`) are allowed plain.

**Write path:** REST `POST/PUT/DELETE /v1/push-providers[/{name}]` (permissions `push-provider.create/update/delete`, audit `push-provider.created/updated/deleted`) or the MCP tools `keeper.push-provider.{create,update,delete,list,read}`. soulctl: `soulctl push-providers {create,update,delete,list,get}`.

**Read path (the resolver).** `PGFallbackProviderResolver` (`keeper/internal/push/provider_pg.go`) at the start of `setupPushDispatchers` resolves the env-payload params for a discovered SSH plugin:

- PG-row found → returns `params` (empty `{}` is allowed; the plugin starts with defaults).
- PG-row absent and `push.allow_legacy_push_providers: false` (default) → `ErrPushProviderNotConfigured`; the daemon starts the plugin without an env-payload (behavior depends on the plugin: `soul-ssh-static` works with defaults, `soul-ssh-vault` requires params and will fail on its own).
- PG-row absent and `push.allow_legacy_push_providers: true` → a one-time WARN deprecation + fallback to `keeper.yml::push.providers[]` (legacy).

**Hot-reload (spawn-on-change, PM-decision S7-2 #6).** A REST/MCP mutation publishes to the Redis pub/sub topic `push-providers:changed` (`keeper/internal/redis/pushproviderchanged.go`). Each cluster node is subscribed via `SubscribePushProvidersChanged`. On receiving a message the daemon listener (`runPushProviderInvalidationListener`) delegates the actual re-spawn to the method `SshDispatcher.RefreshProvider` (S7-2 closure, ADR-032 amendment 2026-05-27): under `d.mu.Lock` it calls `ProviderRespawner.RespawnProvider`, which closes the old plugin handle and spawns a new one with the updated env-payload `SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS` (resolution via `PGFallbackProviderResolver` — PG-first + legacy fallback). Concurrent SendApply on the hot path are not blocked: they take a snapshot reference to the provider via RLock and ride out to the end of the session. There is no Redis pub/sub persistence: a lost message → lazy re-spawn on the next mutation or a keeper restart; mutations are rare, the staleness window is milliseconds. On a failed re-spawn (a crashed Spawn, a missing discovered binary) the dispatcher goes into a degraded state until the next successful refresh — a subsequent SendApply fails with "SshProvider unavailable", the listener keeps working (ERROR log).

**Audit.** The events `push-provider.created` / `.updated` / `.deleted` (a kebab single-section resource name; payload `{name, params_keys}` without values — records the fact of a mutation without revealing secrets).

**Deprecation policy (PM-decision S7-2 #4).** 1-release WARN → hard-cut: after the next release the inline form `keeper.yml::push.providers[]` is rejected at schema validation as `unknown_key`. Auto-import of legacy `push.providers[]` into PG — NOT in this slice, deferred to S7-4 (requires explicit consent + idempotency).

**What is NOT in S7-2:**
- Auto-import of legacy `keeper.yml::push.providers[]` into PG — S7-4.
- Per-host invalidation (the current publication is cluster-wide; per-host routing — a separate multi-provider routing slice).
- A plugin capabilities manifest with its own `sensitive_keys[]` — post-MVP (the current allow-list is opaque in the pushprovider package).

## S7-3 multi-CA host_ca_refs[] (2026-05-26)

ADR-032 amendment 2026-05-26 (S7-3) extends the single Vault-ref `push.host_ca_ref` to an array of structures `push.host_ca_refs[]` for verifying host keys over SSH. The singular `host_ca_ref` remains under a 1-release WARN deprecation window.

**Grammar:**

```yaml
push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca-prod,  name: trusted-bastion-1 }
    - { ref: vault:secret/keeper/ssh-host-ca-stage, name: trusted-bastion-2 }
```

- `ref` — a vault-ref (`vault:<mount>/<path>`), pointing to a Vault KV with a `public_key` field (the same format as the singular). Plaintext-inline-PEM is forbidden (`vault_ref_invalid`).
- `name` — operator-defined kebab-case, mandatory, must be unique within the set (`duplicate_push_host_ca_name`). Used as the label value in `keeper_push_host_ca_used_total{ca_name=...}` and in diag messages.

**Verify logic.** On each SSH handshake (target and proxy_jump hop) `ssh.CertChecker.IsHostAuthority` OR-checks the marshaled form of the presented host authority against all CAs in the set. A host cert signed by any of them → trusted; otherwise reject (a rejection of TOFU). On a match the callback `OnHostCAMatch(caName)` increments `keeper_push_host_ca_used_total{ca_name=...}` + a debug log. A linear bytes.Equal over the marshaled form inside the handshake — the handshake already does more system work (crypto/network), an index over the form is unnecessary (a closed set of single-digit CAs).

**Backward-compat (S7-3 deprecation window).** With the singular `push.host_ca_ref` filled and `host_ca_refs[]` empty, the daemon at the start of `setupPushDispatchers` auto-adapts the singular into `host_ca_refs[0]` with `name='default'` and writes a one-time WARN. Simultaneous presence of singular + plural → the schema phase rejects it (`mutually_exclusive_keys`). After the window closes (1 release) the singular will be hard-cut at the schema phase as `unknown_key`.

**Per-provider CA-override — DEFERRED.** In the MVP all-providers-trust-all-CAs: one `host_ca_refs[]` set applies to all SSH handshakes (target + proxy_jump). A separate CA per provider/route — a post-MVP open Q after S7.

**PM-decisions S7-3:** (1) the format is an array of structures (not a flat array of strings) to extract `name` as a label value; (2) backward-compat singular auto-adapt into a singleton + WARN; (3) mutually exclusive singular+plural; (4) verify via `CertChecker.IsHostAuthority` OR-loop; (5) per-provider CA-override deferred (all-trust-all in the MVP); (6) deprecation policy — 1 release WARN (like S7-1/S7-2).

## S7-4 auto-import legacy on start (2026-05-26)

[ADR-032 amendment 2026-05-26 (S7-4)](../adr/0032-push-orchestrator.md) closes the migration loop: the operator enables a single pass of migrating inline `keeper.yml::push` blocks into the PG sources via flags in `keeper.yml` itself. The pilot and canon keep coexisting (the resolvers are PG-first + fallback under `allow_legacy_push_*`), and auto-import is a separate opt-in, not wired into the resolvers' operation.

**Grammar.** Two new fields, both default `false` (without the operator's explicit consent, silent data migration is forbidden):

```yaml
push:
  # … host_ca_refs[], targets[], providers[] (legacy / canon) …

  # S7-4: opt-in one-shot migration at Keeper start.
  auto_import_legacy_targets:    true   # push.targets[]    → souls.ssh_target jsonb
  auto_import_legacy_providers:  true   # push.providers[]  → push_providers PG-table

  # S7-1/S7-2: legacy-fallback when the PG-row is absent.
  # With allow_legacy_*=false (default) and auto_import_*=false — the yml is ignored
  # by the resolvers; enabling auto_import_* is the recommended migration path.
  allow_legacy_push_targets:     false
  allow_legacy_push_providers:   false
```

**One-shot semantics.** The import runs as the step `runLegacyAutoImport` in the `keeper run` pipeline after `setupPushProviderSvc` (the CRUD facade and audit writer are already up) and BEFORE `setupAPIServer` (the imported rows are immediately visible via REST/MCP). Idempotent:

- `targets[]` — for each SID `souls.ssh_target` is read; `IS NULL` → INSERT + audit; not NULL → skip (PG canonical, we do not overwrite).
- `providers[]` — for each `name`, `SelectByName` is tried; `ErrPushProviderNotFound` → INSERT with `created_by_aid='archon-system'` + audit; the record exists → skip.
- A repeated start without new records in `keeper.yml` — a no-op (not a single write).
- A missing `souls` row for a config-target SID — WARN-skip (not fatal): the soul is not yet registered, importing the detail is meaningless; the operator will later PUT it via `/v1/souls/{sid}/ssh-target`.
- A PG read/write fail (one of the reads / UPDATE / INSERT) → `errSetupFailed`. Already-imported rows remain; the non-imported ones will be picked up on the next start.
- An audit write fail — best-effort WARN, the import of subsequent records continues (storage is already committed; the operator resolves the audit↔storage mismatch manually, the pattern `bootstrap.ErrAuditWriteFailed`).

**Audit events** (a new `source: config_bootstrap`, `archon_aid: NULL`):

| Name | When written | Payload |
|---|---|---|
| `soul.ssh-target.imported_from_config` | per-row, after a successful `UpdateSshTarget`. | `{sid, ssh_port, ssh_user, soul_path}` — cleartext (a mirror of `soul.ssh-target.updated`). |
| `push-provider.imported_from_config` | per-row, after a successful `pushprovider.Insert`. | `{name, params_keys}` — the list of params keys, WITHOUT values (symmetric with `push-provider.created`: sensitive values are not written to audit, the policy is uniform). |

**System-AID `archon-system`.** The imported `push_providers` rows carry `created_by_aid='archon-system'` (to separate them from Archon creates). The FK to `operators(aid)` requires the `archon-system` row to exist in the registry before the first auto-import; the S7-4 pilot assumes the operator adds it by hand (`POST /v1/operators` with `aid=archon-system, auth_method=none`) or it will be created in the bootstrap semantics of the next slice (a separate effort).

**Deprecation timeline.**

- **S7 wave (current release):** the pilot and canon coexist. PG-first resolvers. yml fallback under `allow_legacy_push_*` (default false). Auto-import under `auto_import_legacy_*` (default false). A 1-release WARN on the use of any legacy fallback and on the auto-adapt of the singular `host_ca_ref`.
- **S8 (after the next prod release):** hard-cut. `keeper.yml::push.targets[]` / `push.providers[]` / the singular `host_ca_ref` → `unknown_key` at the schema phase; the `allow_legacy_push_*` / `auto_import_legacy_*` flags are removed; the PG resolvers are simplified to PG-only (without the `Fallback`/`AllowLegacy` fields).

**Operator-driven migration path** (if auto-import does not fit — for example, the operator wants to review the imported data before writing):

- `soulctl souls ssh-target set <sid> --port … --user … --soul-path …` (S7-1).
- `soulctl push-providers create <name> --params=…` (S7-2).
- After the PG data is created — remove the inline blocks from `keeper.yml`.

**PM-decisions S7-4:** (1) auto-import — opt-in (default false, explicit operator consent); (2) one-shot + idempotent (PG-row presence check, not ON CONFLICT — created/skipped must be distinguished for audit); (3) audit events `soul.ssh-target.imported_from_config` + `push-provider.imported_from_config`; (4) a new `source` enum `config_bootstrap` (to separate it from `keeper_internal` semantically); (5) deprecation 1-release WARN → S8 hard-cut; (6) system-AID `archon-system` for the imported `push_providers` rows.

**What is NOT in S7-4:** the S8 hard-cut (a separate slice after the prod release); migration of the reserved-AID `archon-system` (a separate slice). The TODO `push-providers:changed` re-spawn of the SshDispatcher plugin-handle is **closed** in ADR-032 amendment 2026-05-27 (S7-2 closure), see the "S7-2 migration" section above.

## P2 Multi-provider routing (2026-05-27)

[ADR-032 amendment 2026-05-27](../adr/0032-push-orchestrator.md) moves push from the single-provider pilot (one SshProvider per keeper) to an eager spawn of ALL discovered SshProvider plugins + per-SID routing of them between Souls. The operator brings up, for example, `soul-ssh-vault` and `soul-ssh-static` at the same time and routes SIDs between them.

### Selector R1 — 3-tier resolve

The router (`keeper/internal/push/router.go::PGRouter`) resolves the SshProvider plugin name per-SID in three levels:

1. **Level 1: `souls.ssh_target.ssh_provider`** (per-SID explicit). An optional field in the jsonb-shape `souls.ssh_target` (migration 056). When set — it always wins. `source: soul` in audit.
2. **Level 2: `push.coven_default_providers: { <coven>: <provider_name> }`** (per-coven default). A map in `keeper.yml`, hot-reload-aware. Tiebreak on a multiple coven-match — **alphabetical order of coven names** (determinism). `source: coven`.
3. **Level 3: `push.cluster_default_provider: <name>`** (cluster fallback). A scalar in `keeper.yml`. `source: cluster`.

All three levels empty → **`ErrProviderNotRouted`** → fail per-host (status="error", error_code="provider_not_routed"). **WITHOUT provider-chain fallback** (a security invariant: the auth perimeter of different providers differs, a silent fallback breaks trust).

### α-compat: per-job preset

The REST body / MCP args of `POST /v1/push/apply` carry an optional field `ssh_provider` — the old pilot parameter from S6. P2 keeps compatibility:

- The field is set → the preset is applied to ALL SIDs of the run, the **router is NOT invoked** (a per-job override). The audit source is treated as `soul` (per-SID explicit semantically).
- The field is empty → per-SID router resolution by the rules above.

### Eager spawn

All SshProvider plugins that passed `pluginhost.Discover` + `FilterByCatalog` are **spawned at Keeper start** in one wave in `setupPushDispatchers`. Arguments:

- **UX predictability:** the operator sees at start that all configured providers work. A lazy-spawn delay on the first push run worsens the first-SLA.
- **Plugin start cost is a one-time cost:** even 5+ SshProvider plugins come up in parallel within single-digit seconds (handshake + Sigil-verify); the secondary RPCs (Authorize/Sign) are instant.

On a spawn-fail of any plugin — `errSetupFailed` (the operator explicitly declared it in `plugins.ssh_providers[]`, silently ignoring it is not allowed).

### Audit and metrics

- **Audit:** a routing decision is **NOT** written as a separate event (excessive noise with N_SIDs in a run). The actual SshProvider per-SID is saved in **`push_runs.summary.hosts[sid].ssh_provider`** — an operator query via `GET /v1/push/{apply_id}`.
- **Metric:** `keeper_push_provider_routed_total{provider, decision_source}` (counter). `decision_source ∈ {soul, coven, cluster}`. Cardinality-safe: ~N_providers × 3 = single-digit series.

### keeper.yml grammar

```yaml
push:
  # ... host_ca_refs[], host_ca_ref (legacy), allow_legacy_push_*, providers[] (legacy), targets[] (legacy) ...

  # P2 Multi-provider routing (Level 2 + Level 3). Hot-reload-aware.
  coven_default_providers:
    prod:    vault-bastion
    stage:   static-stage
    eu-west: static-eu

  cluster_default_provider: static-fallback
```

### Operator surface

- **Per-SID:** `PUT /v1/souls/{sid}/ssh-target` (extended body: `ssh_provider`) → `soulctl souls ssh-target set <sid> ... --ssh-provider=<name>`. The MCP tool `keeper.soul.ssh-target.update` accepts the same field.
- **Bulk per-coven:** `soulctl souls ssh-target bulk-set --coven=<name> --ssh-provider=<name>` (client-side fan-out over list+PUT; a server-side bulk endpoint is not introduced).
- **Cluster / per-coven default:** editing `keeper.yml::push.{coven_default_providers,cluster_default_provider}` + SIGHUP / API-reload (hot-reload-aware).

### What is NOT in P2

- **Soul-label-selector** `souls.attributes` (a new entity; propose-and-wait, deferred).
- **Per-job inline `routing_rules`** (a γ-variant per-run override map; post-MVP).
- **Provider-chain fallback** (a silent retry on another provider on connect-fail — a security trade-off, rejected by PM-decision).
- **Lazy spawn** (a UX trade-off, rejected).

**Metrics:**

- `keeper_push_host_ca_used_total{ca_name="<name>"}` (counter) — incremented on every CA match in `hostCertCallback`. Cardinality-safe: the names are fixed in `keeper.yml::push.host_ca_refs[].name` (a closed set of single digits), the kebab-case format is validated by the schema phase.

### When push is **disabled** (fail-open, no start error)

- `plugins.ssh_providers[]` is empty/absent → INFO in the log;
- Discover found no SshProvider plugins (a broken cache / a name mismatch in FilterByCatalog) → WARN;
- the `push:` block or `push.host_ca_ref` is absent → WARN.

In these cases `/v1/push/*` returns 404 (the routes are not mounted), MCP `keeper.push.apply` returns an internal-error "not configured".

### When push **fails Keeper's start** (fail-closed)

- `push.host_ca_ref` is set, but Vault is unavailable / the field is absent / broken PEM.
- The spawn of the first SshProvider plugin failed (Sigil-verify did not pass / handshake timeout / capability mismatch).
- Building the `SshDispatcher` failed (a code inconsistency, not runtime).

See also [`config.md → push`](config.md#push) for the normative grammar of the block.

## See also

- [plugins.md](plugins.md) — the `SshProvider` contract, the common gRPC-stdio mechanism.
- [reaper.md](reaper.md) — the Reaper works over Postgres, not over hosts.
- [config.md](config.md) → `plugins.ssh_providers` — the registry of SSH providers.
- [rbac.md](rbac.md) — RBAC is applied to push uniformly.
- [`../soul/modules.md`](../soul/modules.md) — the module layout on the host and host-side cleanup.
- [architecture.md → Push mode](../architecture.md).
- [architecture.md → Delivery of a SoulSeed token to the host](../architecture.md) — push as the target path for bootstrapping an agent.
