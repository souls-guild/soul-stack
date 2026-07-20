package audit

// EventType is the typed alias for the `audit_log.event_type` column.
// Name convention is `<area>.<action>` (lowercase, dots; see
// docs/naming-rules.md → Audit-events).
//
// The catalog is open: new names are added by an ordinary PR to
// docs/naming-rules.md as each write-path subsystem is normalized. Only
// names already fixed by an ADR (ADR-021 → config.*) are declared typed
// here. Other areas (`operator.*`, `incarnation.*`, `push.*`, `cloud.*`,
// `reaper.*`, `task.*`, `soulprint.*`) are added in M0.4.2+ once their
// initiator is implemented.
type EventType string

const (
	// EventConfigReloadSucceeded — after a successful atomic swap of the
	// new config (ADR-021(g)). payload: `changed_paths`, `correlation_id`.
	EventConfigReloadSucceeded EventType = "config.reload_succeeded"

	// EventConfigReloadFailed — after failed validation (in-memory state
	// unchanged, file not modified) (ADR-021(g)). payload:
	// `validation_errors[]`, `phase`.
	EventConfigReloadFailed EventType = "config.reload_failed"

	// EventOperatorCreated — a new Archon was created. Bootstrap of the
	// first Archon (`keeper init`, ADR-013) writes this with `source:
	// keeper_internal`, `archon_aid: NULL` and `payload.bootstrap_initial:
	// true`; later `operator.create` via the Operator API (M0.6+) carry
	// `source: api` / the creator's `archon_aid` and `bootstrap_initial:
	// false` (omitted).
	EventOperatorCreated EventType = "operator.created"

	// EventOperatorRevoked — an Archon was revoked via the Operator API
	// (`POST /v1/operators/{aid}/revoke`). `source: api`, `archon_aid` is
	// the initiator; payload: `{aid, reason}`. Active JWTs of the revoked
	// Archon keep working until `exp` (ADR-014(d)).
	EventOperatorRevoked EventType = "operator.revoked"

	// EventOperatorTokenIssued — a new JWT was issued for an existing Archon
	// via the Operator API (`POST /v1/operators/{aid}/issue-token`).
	// `source: api`, `archon_aid` is the initiator; payload:
	// `{aid, expires_at}`. The JWT is NOT put in the payload (sensitive,
	// masked even if it leaks in).
	EventOperatorTokenIssued EventType = "operator.token-issued"

	// EventIncarnationCreated — a new runtime instance was created via the
	// Operator API (`POST /v1/incarnations`). `source: api`, `archon_aid`
	// is the initiator; payload: `{name, service, apply_id}`. M0.6c-1 is a
	// stub: audit is written when the incarnation row is inserted; the real
	// `create` scenario run is blocked on M2.x (Soul gRPC infrastructure).
	EventIncarnationCreated EventType = "incarnation.created"

	// EventIncarnationScenarioStarted — an operator started a named scenario
	// against an existing incarnation via the Operator API
	// (`POST /v1/incarnations/{name}/scenarios/{scenario}`, ADR-009).
	// `source: api`, `archon_aid` is the initiator; payload: `{name,
	// scenario, apply_id}`. Async: audit is written on request acceptance
	// (202); the run terminal is recorded by a separate `run.completed`
	// (M2.4).
	EventIncarnationScenarioStarted EventType = "incarnation.scenario_started"

	// EventIncarnationUnlocked — an operator cleared the error_locked status
	// via the Operator API (`POST /v1/incarnations/{name}/unlock`, ADR-009).
	// `source: api`, `archon_aid` is the initiator; payload: `{name,
	// previous_status, reason}`. Unlock neither rolls back nor finishes the
	// hosts — it only clears the lock; the operator takes responsibility for
	// consistency (architecture.md → "Atomicity and error_locked").
	EventIncarnationUnlocked EventType = "incarnation.unlocked"

	// EventIncarnationRerunLast — an operator re-ran the LAST failed scenario
	// out of error_locked via the Operator API / MCP
	// (`POST /v1/incarnations/{name}/rerun-last`, architecture.md →
	// "Atomicity and error_locked"). `source: api` / `mcp`, `archon_aid` is
	// the initiator; payload: `{name, reason, scenario, previous_status,
	// apply_id}` (`scenario` = name of the re-run one: bootstrap `create`/…
	// OR operational add_user/…). Atomically clears error_locked (state is
	// NOT touched — last known-good, snapshot in state_history) and by the
	// same action starts the last failed scenario (error_locked → applying,
	// bypassing ready, under a single FOR UPDATE). A distinct event from
	// `incarnation.unlocked` (manual unlock does not re-run) — the recovery
	// path differs.
	EventIncarnationRerunLast EventType = "incarnation.rerun_last"

	// EventIncarnationUpgradeStarted — an operator initiated moving an
	// incarnation to a new state_schema_version via the Operator API
	// (`POST /v1/incarnations/{name}/upgrade`, ADR-019). `source: api`,
	// `archon_aid` is the initiator; payload: `{name, to_version, apply_id}`.
	// sync-under-202: the migration runs synchronously within the request
	// (one PG transaction, docs/migrations.md §Atomicity), audit is written
	// on request acceptance; per-step state_history snapshots are recorded
	// in the same tx under a shared apply_id.
	EventIncarnationUpgradeStarted EventType = "incarnation.upgrade_started"

	// EventIncarnationDestroyStarted — an operator initiated destroy of an
	// incarnation via the Operator API / MCP (S-D1). `source: api` / `mcp`,
	// `archon_aid` is the initiator; payload: `{name, previous_status,
	// force}`. Written when the incarnation moves to `destroying` (before
	// running the `destroy` teardown scenario — S-D2, and the row DELETE —
	// S-D3). `force: true` means destroy without teardown (direct DELETE,
	// S-D3). The destroy terminal itself is recorded by
	// `incarnation.destroy_completed` / `.destroy_failed`.
	EventIncarnationDestroyStarted EventType = "incarnation.destroy_started"

	// EventIncarnationDestroyCompleted — destroy ran to completion: teardown
	// succeeded on all hosts, the incarnation row is physically removed and
	// archived into incarnation_archive / state_history_archive (S-D3,
	// cascade V3). `source: keeper_internal` (write-path — the scenario
	// runner after the barrier, not HTTP middleware; the initiator's AID is
	// unavailable here, archon_aid column NULL). `correlation_id` is empty.
	// Payload: `{name, force}` — the fact of removal; carries no secrets
	// (state/spec are NOT duplicated in audit, they live in the archive).
	// Written AFTER the archive+DELETE transaction commits; single-winner —
	// only the owner of the destroying transition writes this event
	// (RowsAffected==0 → no-op, event not written).
	EventIncarnationDestroyCompleted EventType = "incarnation.destroy_completed"

	// EventIncarnationHostsUpdated — an Archon edited the declared
	// `spec.hosts[]` of an incarnation via the Operator API
	// (`PATCH /v1/incarnations/{name}/hosts`) — supports three modes:
	// replace (full list replacement), append (add / update role by SID) and
	// remove (drop the given SIDs). `source: api` / `mcp`, `archon_aid` is
	// the initiator. Payload: `{name, mode, old_hosts, new_hosts}` —
	// `old_hosts`/`new_hosts` are the `spec.hosts[]` snapshot before and
	// after (SID + role, not a secret); mode records the operation kind for
	// diagnostics. declared `hosts` is the probe-spec source at bootstrap
	// (ADR-008); an edit changes the resolver's namespacing topology for the
	// next run.
	EventIncarnationHostsUpdated EventType = "incarnation.hosts_updated"

	// EventIncarnationTraitsChanged — an Archon fully replaced the
	// operator-set trait labels of an incarnation (`incarnation.traits`,
	// ADR-060 amend R1) via the Operator API
	// (`PUT /v1/incarnations/{name}/traits`) or the MCP mirror
	// (`keeper.incarnation.traits-set`). incarnation.traits is the source of
	// truth that a sync-hook materializes into `souls.traits` of member
	// hosts; the per-soul bulk API (`POST /v1/souls/traits`) is deprecated in
	// favor of this path. `source: api` / `mcp`, `archon_aid` is the
	// initiator. Payload: `{name, old_keys, new_keys}` — sorted lists of
	// trait KEYS before and after; the trait VALUES themselves are NOT put in
	// the payload (they may carry host infrastructure data — the audit trail
	// records the fact of mutation and the key set, not the content,
	// symmetric to `soul.traits-changed`).
	EventIncarnationTraitsChanged EventType = "incarnation.traits_changed"

	// EventIncarnationDestroyFailed — teardown (the `destroy` scenario)
	// failed on the hosts: the instance is NOT removed, the incarnation moves
	// to `destroy_failed` (state stays last known-good). `source:
	// keeper_internal` (write-path — the scenario runner on teardown failure,
	// archon_aid column NULL). `correlation_id = apply_id`. Payload: `{name,
	// apply_id, reason}` — `reason` is masked (the cause may transit a
	// vault-ref). Symmetric to `incarnation.destroy_completed`: both record
	// the destroy terminal, differing by teardown outcome.
	EventIncarnationDestroyFailed EventType = "incarnation.destroy_failed"

	// EventIncarnationSecretRevealed — an operator revealed an incarnation
	// secret via the Operator API
	// (POST /v1/incarnations/{name}/secrets/reveal). `archon_aid` is the
	// initiator; payload `{name, secret_id, key, path}` — the secret VALUE is
	// NOT included (the fact, not the content).
	EventIncarnationSecretRevealed EventType = "incarnation.secret_revealed"

	// EventSoulCreated — a Soul was registered in the `souls` registry via
	// the Operator API (`POST /v1/souls`): a row was created (status:
	// pending) and, for transport=agent, the first bootstrap token was
	// issued. `source: api`, `archon_aid` is the initiator. Payload: `{sid,
	// transport, covens, created_by_aid, token_issued}` — the plain token is
	// NOT put in the payload (sensitive; even under a `bootstrap_token` key
	// it would be masked).
	EventSoulCreated EventType = "soul.created"

	// EventSoulTokenIssued — a new bootstrap token was issued for an existing
	// Soul via the Operator API (`POST /v1/souls/{sid}/issue-token`).
	// `source: api`, `archon_aid` is the initiator. Payload: `{sid, force,
	// expired_previous, expires_at}` — `expired_previous` = true if a
	// force-reissue invalidated a previously active token. Token identifiers
	// are NOT put in the payload: secret-mask (H1) redacts any key with a
	// `token` substring, correlation goes by sid + time. The plain token is
	// of course not included.
	EventSoulTokenIssued EventType = "soul.token-issued"

	// EventSoulBootstrapped — a Soul completed onboarding via the `Bootstrap`
	// gRPC RPC (docs/soul/onboarding.md): the bootstrap token was burned, the
	// CSR signed, a SoulSeed issued and stored, and the Soul status moved
	// `pending → connected`. `source: soul_grpc`, `archon_aid: NULL`,
	// `correlation_id` = token_id. Payload: `{sid, token_id, seed_id,
	// fingerprint, not_after}`.
	EventSoulBootstrapped EventType = "soul.bootstrapped"

	// EventSoulSeedIssued — a new SoulSeed certificate was issued (as part of
	// `Bootstrap` or via the future `SeedRotation` RPC, M2.6). `source:
	// soul_grpc`, `archon_aid: NULL`. Payload: `{sid, seed_id, fingerprint,
	// serial_number, issued_at, not_after, kid}`. At bootstrap it is written
	// together with `soul.bootstrapped` (one correlation_id); on rotation —
	// standalone.
	EventSoulSeedIssued EventType = "soul.seed-issued"

	// EventTaskExecuted — an apply-run task finished. A single name for all
	// terminal statuses (`ok`/`changed`/`failed`/`timed_out`/`skipped`) —
	// status is carried in `payload.status` so that filtering in
	// `GET /v1/audit` goes by it rather than by a spread of event_type.
	// `correlation_id = apply_id`. Payload (common shape,
	// [BuildTaskExecutedPayload]): `{sid, apply_id, task_idx, status, error?,
	// register_data?}` — `error` only on FAILED/TIMED_OUT; `register_data` is
	// masked by the common secret rules.
	//
	// Emitted by BOTH sides (ADR-052 amend §k/§l): Soul-side tasks — M2.4
	// event handler `TaskEvent`, `source: soul_grpc`, host `sid`; keeper-side
	// `on: keeper` tasks (`scenario.dispatchKeeperTasks`) — `source:
	// keeper_internal`, `sid = keeper`. Without keeper-side emission a changed
	// keeper task would drop out of the changed_tasks rollup and the Tiding
	// task subscription. The keeper-side payload carries no register_data
	// (secret hygiene).
	EventTaskExecuted EventType = "task.executed"

	// EventRunCompleted — the final apply-run report (M2.4 event handler
	// `RunResult`). A single name for all RunStatus values
	// (`success`/`failed`/`cancelled`/`error_locked`) — status in
	// `payload.status`. `source: soul_grpc`, `correlation_id = apply_id`.
	// Payload: `{sid, apply_id, status, incarnation?, scenario?, history_id?}`.
	EventRunCompleted EventType = "run.completed"

	// EventIncarnationRunCompleted — the terminal of a scenario-run for one
	// incarnation (T3/T4 foundation, ADR-052 §k): the per-incarnation run
	// result, emitted by scenario.Runner at the terminal of an ordinary run.
	// One event per incarnation-run, NOT per-host (distinct from
	// `run.completed`, which is the per-host RunResult from the Soul).
	// `source: keeper_internal` (write-path — the scenario runner, archon_aid
	// column NULL), `correlation_id = apply_id`.
	//
	// Emitted via TWO paths: on a SUCCESSFUL finish (after the barrier, next
	// to commitSuccess) with `status: success`, AND on a TERMINAL FAILURE of
	// an ordinary run (after lockIncarnation, single-winner only) with
	// `status: failed`. TerminalDestroy reaches neither point — destroy has
	// its own terminal (`incarnation.destroy_completed` / `.destroy_failed`).
	//
	// Payload: `{incarnation, scenario, apply_id, status, changed_tasks,
	// cadence_id?, voyage_id?}`. `status` ∈ {`success`, `failed`}
	// (error_locked folds into `failed` — no sub-statuses). `changed_tasks` =
	// array of `{idx, name, register, id, module, changed_hosts, total_hosts}`
	// for tasks that changed on at least one host (source = aggregate of
	// audit_log over `task.executed`+CHANGED, loop-folded by register∪id
	// address, ADR-052 §j); on failure it is partial (late abort, whatever
	// reached CHANGED) or empty (early abort before render). `cadence_id` is
	// present ONLY when the run was spawned by a Cadence schedule (child
	// Voyage, ADR-046) — a manual run carries no key (conservative, like the
	// drift payload). `voyage_id` is present ONLY when the run was spawned by
	// the Voyage orchestrator (ADR-052 amend §k, visibility) — the direct
	// create/rerun/destroy paths bypass Voyage and carry no key (symmetric
	// with cadence_id); filtered in GET /v1/audit via `payload_voyage` for the
	// Voyage detail. Secret hygiene: changed_tasks carries ONLY task metadata
	// and counts, register/params values are absent from it.
	EventIncarnationRunCompleted EventType = "incarnation.run_completed"

	// EventSoulprintReceived — a `SoulprintReport` was received from a Soul
	// (ADR-018, M2.4 event handler `SoulprintReport`). `source: soul_grpc`,
	// `correlation_id` is empty (not part of the apply chain). Payload:
	// `{sid, collected_at, received_at, has_typed_facts}` — the facts
	// themselves are NOT duplicated in audit, they live in
	// `souls.soulprint_facts`.
	EventSoulprintReceived EventType = "soulprint.received"

	// EventInputVaultResolved — the scenario runner resolved (or rejected) a
	// `vault:` ref in operator input through the scoped channel (docs/input.md
	// → "vault_scope"). `source: keeper_internal`, `archon_aid: NULL`
	// (write-path — the async scenario runner, not HTTP middleware; the
	// initiator's aid is in the payload). A single name for ok and denied —
	// the result is in `payload.result` (`ok`/`denied`) so filtering goes by
	// it rather than by a spread of event_type. A denied resolve is a security
	// signal, audited on par with ok. Payload: `{field, incarnation, scenario,
	// result, aid?, path?, reason?}` — `path` (the logical Vault path) is not
	// a secret and is logged; the secret value is NOT included; `reason`
	// (`no_scope`/`out_of_scope`/`deny_list`/…) only on denied.
	EventInputVaultResolved EventType = "input.vault_resolved"

	// EventVaultKVRead — the keeper-side core module `core.vault.kv-read`
	// (ADR-017) read a secret from Vault KV. `source: keeper_internal`,
	// `archon_aid: NULL`. Payload: `{path, fields}` — the path and the list of
	// requested keys; the secret values themselves are **not** put in the
	// payload (sensitive; the audit trail records the fact of reading, not the
	// content).
	EventVaultKVRead EventType = "vault.kv-read"

	// EventVaultKVPresent — the keeper-side core module `core.vault.kv-present`
	// (ADR-017) generated missing secrets in Vault KV (generate-if-absent).
	// `source: keeper_internal`, `archon_aid: NULL`. Written ONLY when
	// something was actually generated (changed=true); a no-op (all secrets
	// already present) produces no audit event. Payload: `{paths}` — `paths` =
	// map `<vault-path>` → list of generated FIELDS; the generated VALUES
	// themselves are **never** put in the payload (security invariant ADR-010:
	// a new secret must not surface in the audit trail / logs / OTel, only the
	// fact of generation is recorded).
	EventVaultKVPresent EventType = "vault.kv-present"

	// EventCertRegistered — the keeper-side core module `core.cert.registered`
	// (cert-rotation Variant 1, E1) recorded a SERVICE TLS cert of an
	// incarnation into the Warrant registry (so Reaper can see its expiry and
	// rotate it). `source: keeper_internal`, `archon_aid: NULL`,
	// `correlation_id = incarnation`. Written ONLY when something was actually
	// recorded (changed=true; the same fingerprint already registered → no-op,
	// no event). Payload: `{incarnation, certs}` — `certs` = list of `{kind,
	// fingerprint, serial_number, not_after}` (non-secret metadata; the
	// PEM/private key itself is not put in the payload, the module reads only
	// the public cert).
	EventCertRegistered EventType = "cert.registered"

	// EventCertRotated — the Reaper rule `rotate_due_certs` (cert-rotation
	// Variant 1) rotated an expiring service cert of an incarnation: Keeper
	// generated a new keypair+CSR (R2), signed it via Vault PKI, stored the
	// material in Vault, wrote a new active Warrant row (old → superseded) and
	// spawned a Voyage of the rotate_tls operational scenario to deliver the
	// new PEM to the hosts. Area `cert.*` (keeper-side lifecycle, parity with
	// `voyage.reclaimed`). `source: keeper_internal`, `archon_aid: NULL`,
	// `correlation_id = voyage_id`. Payload: `{incarnation, kind, voyage_id,
	// fingerprint, serial_number, not_after, superseded_cert_id,
	// superseded_serial}` — metadata of the NEW cert plus the cert_id/serial of
	// the superseded one (superseded_cert_id is always present); non-secret
	// fields only (the private key/PEM is never included).
	EventCertRotated EventType = "cert.rotated"

	// EventCertIssued — the keeper-side core module `core.cert.issued` (NIM-99)
	// itself ISSUED a server TLS cert for an incarnation: generated the
	// keypair+CSR (R2), signed via Vault PKI with a role from the manifest,
	// stored cert/key in Vault, and wrote the active Warrant rows (cert + key
	// companion). Scope `cert.*` (keeper-side lifecycle). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = incarnation`.
	// Payload: `{incarnation, kind, fingerprint, serial_number, not_after}` —
	// non-secret metadata only (the private key/PEM is never included).
	EventCertIssued EventType = "cert.issued"

	// EventSoulCovenChanged — the set of Coven labels of a Soul changed. Two
	// write paths differ by the `source` field:
	//   - scenario path: the keeper-side core module `core.soul.registered`
	//     (docs/keeper/modules.md), per-host. `source: keeper_internal`,
	//     `archon_aid: NULL`. Payload: `{sid, mode, before, after, created}`.
	//     Written only if the set actually changed.
	//   - bulk API: `POST /v1/souls/coven` (bulk append/remove of a single
	//     label by selector). `source: api`, `archon_aid` is the initiator
	//     (from claims, set by the audit middleware). One event for the whole
	//     operation (not per-chunk). Payload: `{mode, label, selector,
	//     matched, changed, status, scope_applied, dry_run, source}`.
	EventSoulCovenChanged EventType = "soul.coven-changed"

	// EventSoulTraitsChanged — the set of operator-set trait labels of a Soul
	// changed (jsonb column `souls.traits`, ADR-060) via the bulk API
	// `POST /v1/souls/traits` (bulk merge/replace/remove by selector).
	// `source: api`, `archon_aid` is the initiator (from claims, set by the
	// audit middleware). One event for the whole operation (not per-chunk).
	// Payload: `{mode, selector, matched, changed, status, scope_applied,
	// dry_run, source, keys}` — `keys` = list of affected trait KEYS (for
	// merge/replace, the keys of the supplied set; for remove, the deleted
	// keys); the trait VALUES themselves are NOT put in the payload (they may
	// carry host infrastructure data — the audit trail records the fact of
	// mutation and the key set, not the content). Symmetric to
	// `soul.coven-changed`, a separate label axis.
	EventSoulTraitsChanged EventType = "soul.traits-changed"

	// EventCloudProvisioned — the keeper-side core module
	// `core.cloud.provisioned` (ADR-017) created or destroyed a VM via a
	// CloudDriver plugin. `source: keeper_internal`, `archon_aid: NULL`.
	// Payload: `{action, provider, profile, count, vm_ids}` — `action` ∈
	// `created`/`destroyed`. Cloud credentials are not included.
	EventCloudProvisioned EventType = "cloud.provisioned"

	// EventBootstrapDelivered — the keeper-side core module
	// `core.bootstrap.delivered` (ADR-063) delivered a per-VM bootstrap token
	// over SSH to freshly created cloud-init VMs. `source: keeper_internal`,
	// `archon_aid: NULL`. Payload: `{action: "delivered", ssh_provider, count,
	// sids}` — WITHOUT tokens (the plain token itself is visible only in the
	// register of the previous step `core.cloud.created` and is masked on all
	// of its outputs; it does not reach here).
	EventBootstrapDelivered EventType = "bootstrap.delivered"

	// EventApplyDispatched — Keeper sent an `ApplyRequest` to a Soul over the
	// EventStream (M2.5, outbound direction). `source: soul_grpc`,
	// `archon_aid: NULL`, `correlation_id = apply_id`. Payload:
	// `{sid, apply_id, tasks_count}` — the task list is not duplicated, it
	// materializes through `task.executed` events as the run proceeds.
	EventApplyDispatched EventType = "apply.dispatched"

	// EventApplyCancelled — Keeper sent a `CancelApply` to a Soul (M2.5).
	// `source: soul_grpc`, `archon_aid: NULL`, `correlation_id = apply_id`.
	// Payload: `{sid, apply_id, reason}`. Soul-side handling (the actual
	// cancellation of the in-flight ApplyRunner) is recorded by a separate
	// `run.completed` with `status: CANCELLED`.
	EventApplyCancelled EventType = "apply.cancelled"

	// EventLeaseForceReleased — a presence-gated Keeper instance seized a SID
	// lease from a PROVABLY DEAD prev-holder on a Soul reconnect (ADR-027
	// amend (n), recovery backstop S2). A security-sensitive lease-ownership
	// change: the prev-holder is confirmed dead via Conclave presence
	// ([redis.InstanceAlive]), then a CAS-by-prev-holder re-seized the key.
	// `source: soul_grpc`, `archon_aid: NULL`, `correlation_id = sid`.
	// Payload: `{sid, prev_kid, new_kid}`. Written ONLY on a successful
	// force-release (a split-brain rejection / fail-safe is not audited — that
	// is the normal "let the Soul retry").
	EventLeaseForceReleased EventType = "eventstream.lease_force_released"

	// EventSoulSeedRotated — a Soul initiated seed rotation via a
	// `SeedRotationRequest` on the EventStream, Keeper issued a new cert and
	// superseded the previous active one (M2.5, ADR-012). `source: soul_grpc`,
	// `archon_aid: NULL`. Payload: `{sid, seed_id, fingerprint, serial_number,
	// not_after, kid, superseded_seed_id?}`. Symmetric to `soul.seed-issued`
	// (ADR-014), differing only by trigger — here the initiator is the Soul,
	// at bootstrap it is the Keeper-side flow.
	EventSoulSeedRotated EventType = "soul.seed-rotated"

	// EventRoleCreated — an RBAC role was created via the Operator API
	// (`POST /v1/roles`) or the MCP tool `keeper.role.create` (RBAC Slice 2).
	// Authorization changes must be audited (ADR-022). `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name, permissions,
	// created_by_aid}` — permission strings are not a secret and are logged.
	EventRoleCreated EventType = "role.created"

	// EventRoleDeleted — an RBAC role was deleted via the Operator API
	// (`DELETE /v1/roles/{name}`) or the MCP tool `keeper.role.delete` (RBAC
	// Slice 2). `source: api` or `mcp`, `archon_aid` is the initiator. Payload:
	// `{name}`. The role's permissions + membership are cascade-deleted.
	EventRoleDeleted EventType = "role.deleted"

	// EventRolePermissionsUpdated — the permission set of an RBAC role was
	// replaced via the Operator API (`PATCH /v1/roles/{name}/permissions`) or
	// the MCP tool `keeper.role.update` (RBAC Slice 2, replace semantics).
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload: `{name,
	// permissions}` — permission strings are not a secret and are logged.
	EventRolePermissionsUpdated EventType = "role.permissions-updated"

	// EventRoleOperatorGranted — an Archon was bound to an RBAC role via the
	// Operator API (`POST /v1/roles/{name}/operators`) or the MCP tool
	// `keeper.role.grant-operator` (RBAC Slice 2). `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name, aid, granted_by_aid}` —
	// AIDs are not a secret and are logged.
	EventRoleOperatorGranted EventType = "role.operator-granted"

	// EventRoleOperatorRevoked — an Archon was unbound from an RBAC role via
	// the Operator API (`DELETE /v1/roles/{name}/operators/{aid}`) or the MCP
	// tool `keeper.role.revoke-operator` (RBAC Slice 2). `source: api` or
	// `mcp`, `archon_aid` is the initiator. Payload: `{name, aid}` — AIDs are
	// not a secret and are logged.
	EventRoleOperatorRevoked EventType = "role.operator-revoked"

	// EventSynodCreated — a Synod group was created (ADR-049) via the Operator
	// API (`POST /v1/synods`) or the MCP tool `keeper.synod.create`. RBAC
	// topology changes must be audited (ADR-022). `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name, created_by_aid}`.
	EventSynodCreated EventType = "synod.created"

	// EventSynodUpdated — the description of a Synod group was changed (ADR-049
	// amend) via the Operator API (`PATCH /v1/synods/{name}`) or the MCP tool
	// `keeper.synod.update`. Only the description changes (name (PK) is
	// immutable); it grants/revokes no rights, but the RBAC-topology mutation
	// is audited (ADR-022) symmetric to synod.created. `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name, description}` — the
	// description is not a secret and is logged.
	EventSynodUpdated EventType = "synod.updated"

	// EventSynodDeleted — a Synod group was deleted (ADR-049) via the Operator
	// API (`DELETE /v1/synods/{name}`) or the MCP tool `keeper.synod.delete`.
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload:
	// `{name}`. The group's membership + bundle are cascade-deleted.
	EventSynodDeleted EventType = "synod.deleted"

	// EventSynodOperatorAdded — an Archon was added to a Synod group (ADR-049)
	// via the Operator API (`POST /v1/synods/{name}/operators`) or the MCP
	// tool `keeper.synod.add-operator`. The member receives the group's whole
	// role bundle. `source: api` or `mcp`, `archon_aid` is the initiator.
	// Payload: `{name, aid, added_by_aid}` — AIDs are not a secret.
	EventSynodOperatorAdded EventType = "synod.operator-added"

	// EventSynodOperatorRemoved — an Archon was removed from a Synod group
	// (ADR-049) via the Operator API
	// (`DELETE /v1/synods/{name}/operators/{aid}`) or the MCP tool
	// `keeper.synod.remove-operator`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{name, aid}`.
	EventSynodOperatorRemoved EventType = "synod.operator-removed"

	// EventSynodRoleGranted — a role was added to a Synod group's bundle
	// (ADR-049) via the Operator API (`POST /v1/synods/{name}/roles`) or the
	// MCP tool `keeper.synod.grant-role`. All group members receive the role's
	// effective rights. `source: api` or `mcp`, `archon_aid` is the initiator.
	// Payload: `{name, role, granted_by_aid}`.
	EventSynodRoleGranted EventType = "synod.role-granted"

	// EventSynodRoleRevoked — a role was removed from a Synod group's bundle
	// (ADR-049) via the Operator API
	// (`DELETE /v1/synods/{name}/roles/{role_name}`) or the MCP tool
	// `keeper.synod.revoke-role`. The role's rights are removed from all group
	// members. `source: api` or `mcp`, `archon_aid` is the initiator. Payload:
	// `{name, role}`.
	EventSynodRoleRevoked EventType = "synod.role-revoked"

	// EventPluginAllowed — an Archon admitted a plugin into the `plugin_sigils`
	// allow-list (Sigil, ADR-026) via the Operator API
	// (`POST /v1/plugins/sigils`) or the MCP tool (S4b). `source: api` or
	// `mcp`, `archon_aid` is the initiator. Payload: `{namespace, name, ref,
	// sha256, allowed_by_aid}` — a supply-chain event, must be audited;
	// signature/manifest are NOT put in the payload (crypto material / large
	// JSONB). `ref` is an operator-asserted label (variant C: NOT git-verified),
	// the integrity authority is sha256+signature.
	EventPluginAllowed EventType = "plugin.allowed"

	// EventPluginRevoked — an Archon revoked a previously admitted plugin from
	// `plugin_sigils` (the binary stops passing Sigil verification) via the
	// Operator API (`DELETE /v1/plugins/sigils/{namespace}/{name}/{ref}`) or
	// the MCP tool (S4b). `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{namespace, name, ref}`. (`plugin.verify_failed` is
	// the host-side verification event, introduced separately in S6.)
	EventPluginRevoked EventType = "plugin.revoked"

	// EventAugurFetchBrokered — the Augur broker (delegate=false, MVP-1,
	// ADR-025 / augur.md §8) read a value from an external system and returned
	// it to the Soul inline. `source: soul_grpc`, `archon_aid: NULL`,
	// `correlation_id = apply_id`. Payload: `{sid, omen, query, request_id}` —
	// records the FACT of reading + the Omen + query (the logical path, not a
	// secret); the value / token itself is NOT put in the payload (augur.md §8,
	// secret-masking ADR-010).
	EventAugurFetchBrokered EventType = "augur.fetch_brokered"

	// EventAugurAccessDenied — any authorization check of an Augur request
	// (augur.md §6) failed: Omen not found / Soul outside the Rite / query
	// outside the allow-list / vault-path normalization rejected the request.
	// A denied resolve is a security signal, audited on par with success.
	// `source: soul_grpc`, `archon_aid: NULL`, `correlation_id = apply_id`.
	// Payload: `{sid, omen, query, request_id, reason}` — `reason` is a
	// human-readable rejection cause; secret values are absent (access did not
	// happen).
	EventAugurAccessDenied EventType = "augur.access_denied"

	// EventServiceRegistered — an Archon registered a Service in the
	// `service_registry` via the Operator API (`POST /v1/services`) or the MCP
	// tool `keeper.service.register` (ADR-028 RBAC-storage pattern). `source:
	// api` or `mcp`, `archon_aid` is the initiator. Payload: `{name, git, ref,
	// created_by_aid}` — the git URL is not a secret and is logged.
	EventServiceRegistered EventType = "service.registered"

	// EventServiceUpdated — an Archon replaced the mutable fields of a Service
	// record (git/ref/refresh, replace semantics) via the Operator API
	// (`PATCH /v1/services/{name}`) or the MCP tool `keeper.service.update`.
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload: `{name,
	// git, ref}` — the git URL is not a secret and is logged.
	EventServiceUpdated EventType = "service.updated"

	// EventServiceDeregistered — an Archon removed a Service record from
	// `service_registry` via the Operator API (`DELETE /v1/services/{name}`) or
	// the MCP tool `keeper.service.deregister`. `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name}`.
	EventServiceDeregistered EventType = "service.deregistered"

	// EventSigilKeyIntroduced — an Archon introduced a new Sigil signing
	// trust-anchor key into the `sigil_signing_keys` registry (ADR-026(h),
	// R3-S7) via the Operator API (`POST /v1/sigil/keys`) or the MCP tool
	// `keeper.sigil.key.introduce`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{key_id, is_primary, introduced_by_aid}` — the
	// private key (in Vault) is NOT put in the payload (security invariant
	// ADR-026(d)). key_id is a stable SHA-256(SPKI), not a secret.
	EventSigilKeyIntroduced EventType = "sigil.key-introduced"

	// EventSigilKeyRetired — an Archon retired a signing trust-anchor key from
	// `sigil_signing_keys` (a Soul forgets it on the next SigilTrustAnchors)
	// via the Operator API (`DELETE /v1/sigil/keys/{key_id}`) or the MCP tool
	// `keeper.sigil.key.retire`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{key_id, retired_by_aid}`.
	EventSigilKeyRetired EventType = "sigil.key-retired"

	// EventSigilKeyPrimarySet — an Archon made an active key primary (new
	// Sigils are signed with it after the R3-S6 reload) via the Operator API
	// (`POST /v1/sigil/keys/{key_id}/primary`) or the MCP tool
	// `keeper.sigil.key.set-primary`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{key_id, set_by_aid}`.
	EventSigilKeyPrimarySet EventType = "sigil.key-primary-set"

	// EventOmenCreated — an Archon created an Omen record in the `omens`
	// registry (the external Augur system, ADR-025 / augur.md §4.1) via the
	// Operator API (`POST /v1/augur/omens`) or the MCP tool
	// `keeper.augur.omen.create`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{name, source_type, endpoint, auth_ref,
	// created_by_aid}` — endpoint (the external system URL) and auth_ref (a
	// vault-ref, not the secret itself) are not a secret and are logged; the
	// master credential is NOT put in the payload (it is not in the record —
	// only a reference, augur.md §4.1).
	EventOmenCreated EventType = "omen.created"

	// EventOmenRevoked — an Archon deleted an Omen record from `omens` via the
	// Operator API (`DELETE /v1/augur/omens/{name}`) or the MCP tool
	// `keeper.augur.omen.delete`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{name}`. All related Rites are removed by cascade
	// (ON DELETE CASCADE) (augur.md §9).
	EventOmenRevoked EventType = "omen.revoked"

	// EventRiteCreated — an Archon created a Rite record (grant) in the `rites`
	// registry (ADR-025 / augur.md §4.2) via the Operator API
	// (`POST /v1/augur/rites`) or the MCP tool `keeper.augur.rite.create`.
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload: `{id,
	// omen, subject, delegate, created_by_aid}` — `subject` is the
	// human-readable subject form (`coven=<v>` / `sid=<v>`); the `allow` list
	// is NOT put in the payload (its shape depends on source_type and carries
	// no secrets, but we record the minimal grant field set).
	EventRiteCreated EventType = "rite.created"

	// EventRiteRevoked — an Archon deleted a Rite record from `rites` via the
	// Operator API (`DELETE /v1/augur/rites/{id}`) or the MCP tool
	// `keeper.augur.rite.delete`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{id}`.
	EventRiteRevoked EventType = "rite.revoked"

	// EventVigilCreated — an Archon created a Vigil record in the `vigils`
	// registry (a Soul-side check of the beacons loop, ADR-030) via the
	// Operator API (`POST /v1/vigils`) or the MCP tool
	// `keeper.oracle.vigil.create`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{name, check, interval, subject,
	// created_by_aid}` — `subject` is the human-readable form (`coven=<v>` /
	// `sid=<v>`); params (the check configuration) are NOT put in the payload
	// (minimal field set).
	EventVigilCreated EventType = "vigil.created"

	// EventVigilDeleted — an Archon deleted a Vigil record from `vigils` via
	// the Operator API (`DELETE /v1/vigils/{name}`) or the MCP tool
	// `keeper.oracle.vigil.delete`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{name}`. The Vigil stops being handed to hosts
	// in the VigilSnapshot; Decrees are NOT cascaded to it (decrees.on_beacon
	// is a text reference without an FK, ADR-030).
	EventVigilDeleted EventType = "vigil.deleted"

	// EventDecreeCreated — an Archon created a Decree record (a reactor rule)
	// in the `decrees` registry (ADR-030) via the Operator API
	// (`POST /v1/decrees`) or the MCP tool `keeper.oracle.decree.create`.
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload: `{name,
	// on_beacon, incarnation, action_scenario, subject, created_by_aid}` — not
	// a secret; the where-CEL and action_input are NOT put in the payload
	// (action_input may transit a vault-ref, invariant A ADR-027).
	EventDecreeCreated EventType = "decree.created"

	// EventDecreeDeleted — an Archon deleted a Decree record from `decrees` via
	// the Operator API (`DELETE /v1/decrees/{name}`) or the MCP tool
	// `keeper.oracle.decree.delete`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{name}`. The cooldown state in `oracle_fires`
	// is cleaned by cascade (ON DELETE CASCADE) (ADR-030(a)).
	EventDecreeDeleted EventType = "decree.deleted"

	// EventOracleFired — Oracle matched a Portent against a Decree and queued
	// the named scenario into the work-queue (ADR-030(b), beacons reactor). A
	// reactor firing is a security signal (untrusted Soul input triggered an
	// action) and is audited on each firing. `source: soul_grpc`,
	// `archon_aid: NULL` (Soul-initiated, not an operator),
	// `correlation_id = apply_id` of the queued run. Payload:
	// `{decree, subject, scenario, beacon, apply_id}` — subject = the
	// authoritative SID of the sending host (from the mTLS peer cert);
	// event.data values are NOT put in the payload (they may carry arbitrary
	// data from an untrusted source).
	EventOracleFired EventType = "oracle.fired"

	// EventIncarnationDriftChecked — an operator ran a Scry drift check via
	// REST/MCP (ADR-031, on-demand pilot). `source: api` / `mcp`, `archon_aid`
	// is the initiator; payload: `{name, scenario, apply_id, drift_summary}` —
	// `drift_summary` = `{hosts_drifted, hosts_clean, hosts_unsupported,
	// hosts_failed}` (aggregates of per-host terminals from the DriftReport).
	// The incarnation `drift` status after the event is a separate signal; here
	// it is exactly the fact of running the check and its aggregates that is
	// recorded. sync-under-200: audit is written after assembling the
	// DriftReport, not on request acceptance (parity with destroy_completed —
	// the event is written on the fact, not the initiation). drift is NOT a
	// blocking status (ADR-031(d)).
	EventIncarnationDriftChecked EventType = "incarnation.drift_checked"

	// EventPushApplied — an operator initiated a Destiny push run over SSH via
	// the Operator API (`POST /v1/push/apply`) or the MCP tool
	// `keeper.push.apply` (Variant C orchestrator, docs/keeper/push.md).
	// `source: api` or `mcp`, `archon_aid` is the initiator. Payload:
	// `{apply_id, destiny, inventory_size, ssh_provider, cleanup_stale}` —
	// `destiny` (form `<name>@<ref>`) and `ssh_provider` (the name from
	// keeper.yml::plugins.ssh_providers[].name) are not a secret,
	// `inventory_size` is the SID count (the SIDs themselves are NOT
	// duplicated, they live in push_runs.inventory_sids). Written on request
	// acceptance (status: pending), before executeAsync starts. Terminal —
	// `push.completed` / `push.failed` / `push.partial_failed`.
	EventPushApplied EventType = "push.applied"

	// EventPushCompleted — the terminal of a push run: every per-host
	// SshDispatcher.SendApply returned a RunResult with status SUCCESS.
	// `source: api` or `mcp` (the same as in `push.applied`), `archon_aid` is
	// the initiator. Payload: `{apply_id, destiny, inventory_size, status:
	// "success", total, success_count, fail_count}`. The per-host details
	// (sid, error) themselves are NOT duplicated — they live in
	// push_runs.summary (GET /v1/push/{apply_id}). status is for filtering in
	// `GET /v1/audit` without event_type spread (the
	// `task.executed`/`run.completed` pattern).
	EventPushCompleted EventType = "push.completed"

	// EventPushFailed — the terminal of a push run: no host reached SUCCESS
	// (all per-host SendApply failed or returned a non-SUCCESS RunResult), or
	// the prepare phase failed (inventory_load_failed / render_failed /
	// no_live_hosts / empty_plan). `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{apply_id, destiny, inventory_size, status:
	// "failed", total, success_count, fail_count}` (success_count=0 for a
	// prepare-fail with empty results). Details — push_runs.summary.
	EventPushFailed EventType = "push.failed"

	// EventPushPartialFailed — the terminal of a push run: a mixed outcome
	// (some hosts SUCCESS, some failed/error_locked/delivery-error). `source:
	// api` or `mcp`, `archon_aid` is the initiator. Payload: `{apply_id,
	// destiny, inventory_size, status: "partial_failed", total, success_count,
	// fail_count}`. Per-host details — push_runs.summary.
	EventPushPartialFailed EventType = "push.partial_failed"

	// EventDecreeCircuitTripped — Oracle's circuit breaker auto-disabled a
	// Decree (ADR-030(a), beacons S4): N firings within the window →
	// enabled=false. A Decree mutation (symmetric to
	// `decree.created`/`decree.deleted`), hence the name is in the `decree.*`
	// area. Written ONLY by the single-winner (the instance whose TripDecree
	// won, RowsAffected==1) — exactly one event per trip. `source: soul_grpc`
	// (write-path — the Soul-initiated Portent flow in evaluateDecree, not an
	// operator), `archon_aid: NULL`. Payload: `{decree, fire_count, window,
	// trigger}` — `trigger` is always `"circuit_breaker"`. WITHOUT
	// subject/beacon/event.data: a trip is a property of the rule (the
	// aggregate threshold was exceeded), not of a single host; the untrusted
	// event payload is not included.
	EventDecreeCircuitTripped EventType = "decree.circuit_tripped"

	// EventTypeErrandInvoked — an operator initiated an Errand pull-ad-hoc exec
	// via `POST /v1/souls/{sid}/exec` (ADR-033). `source: api` / `mcp`,
	// `archon_aid` is the initiator; payload: `{sid, module, errand_id,
	// timeout_seconds, dry_run}` — `input` is NOT put in the payload (it may
	// carry vault-resolved secrets after the CEL-render phase).
	EventTypeErrandInvoked EventType = "errand.invoked"

	// EventTypeErrandCompleted — an Errand terminated with status SUCCESS
	// (ADR-033). `source: soul_grpc`, `archon_aid: NULL`. Payload: `{sid,
	// module, errand_id, exit_code, duration_ms, stdout_truncated,
	// stderr_truncated}` — stdout/stderr are NOT put in the payload (they may
	// be large + masking happens on output by the common rules).
	EventTypeErrandCompleted EventType = "errand.completed"

	// EventTypeErrandFailed — an Errand terminated with status FAILED or
	// MODULE_NOT_ALLOWED (ADR-033, Soul-side whitelist reject). `source:
	// soul_grpc`, `archon_aid: NULL`. Payload: `{sid, module, errand_id,
	// exit_code, duration_ms, error_message}` — `error_message` is masked.
	EventTypeErrandFailed EventType = "errand.failed"

	// EventTypeErrandTimedOut — an Errand terminated with status TIMED_OUT
	// (ADR-033, `timeout_seconds` exceeded). `source: soul_grpc`,
	// `archon_aid: NULL`. Payload: `{sid, module, errand_id, duration_ms}`.
	EventTypeErrandTimedOut EventType = "errand.timed_out"

	// EventTypeErrandCancelled — an Archon cancelled an in-flight Errand via
	// `DELETE /v1/errands/{errand_id}` (ADR-033, slice E5 / post-MVP).
	// `source: api` / `mcp`, `archon_aid` is the initiator. Payload:
	// `{errand_id, sid}`.
	EventTypeErrandCancelled EventType = "errand.cancelled"

	// EventClusterDegradedSet — the Toll leader raised the cluster:degraded
	// flag (ADR-038): disconnect rate > threshold within a sliding 60s window.
	// Single-winner (only the leader holding the Redis lease
	// `cluster:toll:leader` writes this event). `source: keeper_internal`
	// (cluster-initiated, not an operator), `archon_aid: NULL`. Payload:
	// `{leader_kid, rate, baseline_connected, threshold, window_seconds}` —
	// numeric parameters, no secrets.
	EventClusterDegradedSet EventType = "cluster.degraded_set"

	// EventClusterDegradedCleared — the Toll leader cleared the
	// cluster:degraded flag (ADR-038): after a sustained rate ≤ threshold over
	// the grace window (asymmetric hysteresis). `source: keeper_internal`,
	// `archon_aid: NULL`. Payload: `{leader_kid, rate, baseline_connected,
	// grace_seconds}`.
	EventClusterDegradedCleared EventType = "cluster.degraded_cleared"

	// EventSoulSshTargetUpdated — an Archon updated the per-host SSH
	// credentials of the push flow (ADR-032 amendment 2026-05-26, S7-1) via
	// the Operator API (`PUT /v1/souls/{sid}/ssh-target`) or the MCP tool
	// `keeper.soul.ssh-target.update`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{sid, ssh_port, ssh_user, soul_path}` — all
	// fields cleartext (port/user/path are not a secret; infrastructure
	// credentials, whose changes need auditing symmetric to coven-changes).
	EventSoulSshTargetUpdated EventType = "soul.ssh-target.updated"

	// EventPushProviderCreated — an Archon created a Push-Provider (per-provider
	// env-payload params of the push-flow SSH plugin, ADR-032 amendment
	// 2026-05-26, S7-2) via the Operator API (`POST /v1/push-providers`) or the
	// MCP tool `keeper.push-provider.create`. `source: api` or `mcp`,
	// `archon_aid` is the initiator. Payload: `{name, params_keys}` —
	// `params_keys` (the list of keys, WITHOUT values) records the fact of
	// mutation without exposing secrets: sensitive values
	// (secret_id/token/password/private_key) must be vault-refs
	// (Service.validateSensitive), non-sensitive ones (vault_addr/role) are
	// not a secret, but the "we don't write values into audit" policy is
	// uniform for symmetry and resilience to a future allow-list expansion.
	EventPushProviderCreated EventType = "push-provider.created"

	// EventPushProviderUpdated — an Archon replaced a Push-Provider's params
	// (replace semantics) via `PUT /v1/push-providers/{name}` or the MCP tool
	// `keeper.push-provider.update`. `source: api`/`mcp`, `archon_aid` is the
	// initiator. Payload: `{name, params_keys}`.
	EventPushProviderUpdated EventType = "push-provider.updated"

	// EventPushProviderDeleted — an Archon deleted a Push-Provider record via
	// `DELETE /v1/push-providers/{name}` or the MCP tool
	// `keeper.push-provider.delete`. `source: api`/`mcp`, `archon_aid` is the
	// initiator. Payload: `{name}`. On the next `push-providers:changed`
	// pub/sub signal SshDispatcher re-spawns the plugin without an env-payload
	// (or with a legacy fallback when allow_legacy_push_providers=true).
	EventPushProviderDeleted EventType = "push-provider.deleted"

	// EventSoulSshTargetImportedFromConfig — one-shot auto-import of a per-host
	// SSH credential of the push flow from `keeper.yml::push.targets[]` into
	// `souls.ssh_target` at Keeper start (ADR-032 amendment 2026-05-26, S7-4).
	// `source: config_bootstrap`, `archon_aid: NULL` (system action). Payload:
	// `{sid, ssh_port, ssh_user, soul_path}` — all fields cleartext
	// (infrastructure credentials, not a secret; mirror of
	// `soul.ssh-target.updated`). Idempotent: the event is written per-row
	// once — a restart with an already-imported SID skips it without an event.
	EventSoulSshTargetImportedFromConfig EventType = "soul.ssh-target.imported_from_config"

	// EventPushProviderImportedFromConfig — one-shot auto-import of Push-Provider
	// env-payload params from `keeper.yml::push.providers[]` into the PG table
	// `push_providers` at Keeper start (ADR-032 amendment 2026-05-26, S7-4).
	// `source: config_bootstrap`, `archon_aid: NULL`. Payload: `{name,
	// params_keys}` — `params_keys` (the list of keys, WITHOUT values) records
	// the fact of mutation without exposing secrets: symmetric with
	// `push-provider.created` (Service.Create audit payload).
	EventPushProviderImportedFromConfig EventType = "push-provider.imported_from_config"

	// EventChoirCreated — an Archon created a Choir record (declared host
	// topology within an incarnation, ADR-044, S-T3) via the Operator API
	// (`POST /v1/incarnations/{name}/choirs`) or the MCP tool
	// `keeper.choir.create`. `source: api` or `mcp`, `archon_aid` = the
	// initiator's JWT.sub (created_by_aid is taken from the context, NOT from
	// the body). Payload: `{incarnation_name, choir_name, min_size?, max_size?,
	// created_by_aid}` — the description/limits are not a secret and are
	// logged.
	EventChoirCreated EventType = "choir.created"

	// EventChoirDeleted — an Archon deleted a Choir record via the Operator API
	// (`DELETE /v1/incarnations/{name}/choirs/{choir}`) or the MCP tool
	// `keeper.choir.delete`. `source: api` or `mcp`, `archon_aid` is the
	// initiator. Payload: `{incarnation_name, choir_name}`. All of the Choir's
	// Voices are removed by cascade (ON DELETE CASCADE).
	EventChoirDeleted EventType = "choir.deleted"

	// EventChoirVoiceAdded — an Archon added a Voice (SID membership in a Choir,
	// ADR-044) via the Operator API
	// (`POST /v1/incarnations/{name}/choirs/{choir}/voices`) or the MCP tool
	// `keeper.choir.add-voice`. `source: api` or `mcp`, `archon_aid` is the
	// initiator (added_by_aid is taken from the context, NOT from the body).
	// Payload: `{incarnation_name, choir_name, sid, role?, position?,
	// added_by_aid}` — role/position omitempty (nullable declared attributes).
	EventChoirVoiceAdded EventType = "choir.voice_added"

	// EventChoirVoiceRemoved — an Archon removed a Voice from a Choir via the
	// Operator API
	// (`DELETE /v1/incarnations/{name}/choirs/{choir}/voices/{sid}`) or the MCP
	// tool `keeper.choir.remove-voice`. `source: api` or `mcp`, `archon_aid` is
	// the initiator. Payload: `{incarnation_name, choir_name, sid}`.
	EventChoirVoiceRemoved EventType = "choir.voice_removed"

	// EventScenarioRunStarted — a Voyage `kind=scenario` was created/started
	// (ADR-043, S5). Semantically REPLACES `tide.started`. Written by the
	// HTTP/MCP handler `POST /v1/voyages` right after a successful INSERT of
	// the pending/scheduled row (parity with `errand_run.invoked` /
	// `tide.started`: an RBAC-mutating event). `source: api` or `mcp`,
	// `archon_aid` = the initiator's JWT.sub. Payload: `{voyage_id, kind,
	// scenario_name, target (declared incarnations[]/service/coven), scope_size
	// (count of resolved incarnations), batch_size, concurrency, dry_run,
	// on_failure}` — `input` is NOT put in the payload (it may carry
	// vault-resolved secrets after the CEL-render phase, invariant A ADR-027).
	// The finalize family (`scenario_run.completed`/`partial_failed`/`failed`/
	// `cancelled`, keeper_internal) is a follow-up once finalize-audit is wired
	// into the VoyageWorker. See the `scenario_run.*` block in
	// `docs/naming-rules.md → Audit-events`.
	EventScenarioRunStarted EventType = "scenario_run.started"

	// EventCommandRunInvoked — a Voyage `kind=command` was created (ADR-043,
	// S5). Semantically REPLACES `errand_run.invoked`. Written by the HTTP/MCP
	// handler `POST /v1/voyages` right after a successful INSERT of the
	// pending/scheduled row. `source: api` or `mcp`, `archon_aid` = the
	// initiator's JWT.sub. Payload: `{voyage_id, kind, module, target (declared
	// sids[]/coven/where), scope_size (count of resolved hosts after AND-merge),
	// batch_size, concurrency, dry_run, on_failure}` — `input` is NOT put in
	// the payload (invariant A ADR-027). The finalize family
	// (`command_run.completed`/`partial_failed`/`failed`/`cancelled`,
	// keeper_internal) is a follow-up once finalize-audit is wired into the
	// VoyageWorker. See the `command_run.*` block in `docs/naming-rules.md →
	// Audit-events`.
	EventCommandRunInvoked EventType = "command_run.invoked"

	// EventScenarioRunCancelled — a Voyage `kind=scenario` was cancelled by an
	// operator (ADR-043, S5): `DELETE /v1/voyages/{id}` for a pending/scheduled
	// run. `source: api` or `mcp`, `archon_aid` = the initiator's JWT.sub.
	// Payload: `{voyage_id, kind, previous_status}` — previous_status records
	// which non-running status the run was moved to cancelled from
	// (running-cancel is post-MVP).
	EventScenarioRunCancelled EventType = "scenario_run.cancelled"

	// EventCommandRunCancelled — a Voyage `kind=command` was cancelled by an
	// operator (ADR-043, S5): `DELETE /v1/voyages/{id}` for a pending/scheduled
	// run. Payload semantics — parity with [EventScenarioRunCancelled].
	EventCommandRunCancelled EventType = "command_run.cancelled"

	// EventScenarioRunLegStarted — the VoyageWorker began executing a
	// kind=scenario Leg (ADR-043, finalize-audit). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Emitted BEFORE the
	// Leg's incarnation fan-out (parity with `tide.surge_started`). Payload:
	// `{voyage_id, kind, leg_index, incarnations_in_leg}`. The command family
	// has no leg events (parity with `errand_run.*` — a flat fan-out without a
	// per-Leg barrier).
	EventScenarioRunLegStarted EventType = "scenario_run.leg_started"

	// EventScenarioRunLegCompleted — the VoyageWorker finished a kind=scenario
	// Leg: all of the Leg's incarnations reached a terminal + the Summary delta
	// was aggregated (parity with `tide.surge_completed`). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`.
	// Payload: `{voyage_id, kind, leg_index, terminal, total, succeeded,
	// failed, cancelled}`.
	EventScenarioRunLegCompleted EventType = "scenario_run.leg_completed"

	// EventScenarioRunCompleted — a Voyage `kind=scenario` was finalized as
	// succeeded (all incarnations success/no_match). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Payload: `{voyage_id,
	// kind, total_batches, summary}` (parity with `tide.completed`).
	EventScenarioRunCompleted EventType = "scenario_run.completed"

	// EventScenarioRunPartialFailed — a Voyage `kind=scenario` was finalized as
	// partial_failed (some incarnations failed, at least one succeeded).
	// `source: keeper_internal`, `archon_aid: NULL`, `correlation_id =
	// voyage_id`. Payload: `{voyage_id, kind, total_batches, summary,
	// on_failure}` (parity with `tide.partial_failed`).
	EventScenarioRunPartialFailed EventType = "scenario_run.partial_failed"

	// EventScenarioRunFailed — a Voyage `kind=scenario` was finalized as failed
	// (nobody succeeded, or fail-closed before the incarnations started:
	// spawner not configured / empty scenario_name / target resolve failed).
	// `source: keeper_internal`, `archon_aid: NULL`, `correlation_id =
	// voyage_id`. Payload: `{voyage_id, kind, total_batches, summary,
	// error_code?}` — `error_code` (∈ `spawner_not_configured`/
	// `empty_scenario_name`/`target_resolve_failed`) only on fail-closed paths,
	// absent when "all incarnations failed".
	EventScenarioRunFailed EventType = "scenario_run.failed"

	// EventScenarioRunLeaseLost — the VoyageWorker lost the lease mid-run for
	// kind=scenario (renewal CAS returned 0 rows — another Keeper picked up the
	// Voyage via reclaim+claim). The orchestrator drops the work, does NOT
	// finalize (parity with `tide.lease_lost`). `source: keeper_internal`,
	// `archon_aid: NULL`, `correlation_id = voyage_id`. Payload: `{voyage_id,
	// kind, kid_who_lost, phase}` — `phase` ∈ `leg`/`finalize`. A command run
	// writes no separate event on lease loss (parity with `errand_run.*`
	// without lease_lost) — the run is silently picked up by another Keeper.
	EventScenarioRunLeaseLost EventType = "scenario_run.lease_lost"

	// EventCommandRunCompleted — a Voyage `kind=command` was finalized as
	// succeeded (all hosts success). `source: keeper_internal`, `archon_aid:
	// NULL`, `correlation_id = voyage_id`. Payload: `{voyage_id, kind, total,
	// succeeded}` (parity with `errand_run.completed`).
	EventCommandRunCompleted EventType = "command_run.completed"

	// EventCommandRunPartialFailed — a Voyage `kind=command` was finalized as
	// partial_failed (some hosts failed, at least one succeeded). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`.
	// Payload: `{voyage_id, kind, total, succeeded, failed, cancelled,
	// on_failure}` (parity with `errand_run.partial_failed`).
	EventCommandRunPartialFailed EventType = "command_run.partial_failed"

	// EventCommandRunFailed — a Voyage `kind=command` was finalized as failed
	// (nobody succeeded, or fail-closed before the hosts started: CommandSpawner
	// not configured / empty module / target resolve failed). `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id = voyage_id`.
	// Payload: `{voyage_id, kind, total, succeeded, error_code?}` — `error_code`
	// (∈ `spawner_not_configured`/`empty_module`/`target_resolve_failed`) only
	// on fail-closed paths. The command family has no leg events (parity with
	// `errand_run.*` — a flat fan-out).
	EventCommandRunFailed EventType = "command_run.failed"

	// EventVoyageReclaimed — the Reaper rule `reclaim_voyages` returned a stale
	// running Voyage back to `pending` (the claiming Keeper instance is dead or
	// went into graceful drain): the row moved `running → pending`,
	// `claimed_by_kid → NULL`, `attempt++`. Area `voyage.*` (NOT
	// `scenario_run.*`/`command_run.*`) — kind-agnostic: the SQL reclaim does
	// not inspect kind, the event is shared by both families. `source:
	// keeper_internal`, `archon_aid: NULL`. Payload: `{voyage_id,
	// last_renewed_at, attempt_after}` (parity with `tide.reclaimed`).
	EventVoyageReclaimed EventType = "voyage.reclaimed"

	// EventReconcileOrphanApplyingExecuted — the Reaper rule
	// `reconcile_orphan_applying` cleared an orphaned applying-lock of an
	// incarnation (ADR-027 amend (m)): a direct (standalone, not under a
	// Voyage) scenario-run of a crashed Keeper owner left
	// `incarnation.status='applying'` forever; the rule detects a stale
	// candidate by the epoch columns (`applying_by_kid`/`applying_since`),
	// confirms the owner's death with a Conclave presence check and clears the
	// lock (`applying → ready` via the idempotent `ReleaseApplyingOrphan`).
	// Area `reaper.*` (a leader recovery action, parity with
	// `voyage.reclaimed`). `source: keeper_internal`, `archon_aid: NULL`.
	// Payload: `{incarnation, prev_kid, apply_id}` — `prev_kid` = the dead
	// `applying_by_kid`, `apply_id` = `applying_apply_id`.
	EventReconcileOrphanApplyingExecuted EventType = "reaper.reconcile_orphan_applying.executed"

	// EventCadenceCreated — an Archon created a Cadence schedule (ADR-046 §8)
	// via the Operator API (`POST /v1/cadences`) or an MCP tool. `source: api`
	// or `mcp`, `archon_aid` = the initiator's JWT.sub (created_by_aid is taken
	// from the context, NOT from the body). Payload: `{cadence_id, name,
	// schedule_kind, kind, scenario_name?, module?, overlap_policy, enabled}` —
	// the recipe's `input` is NOT put in the payload (invariant A ADR-027).
	EventCadenceCreated EventType = "cadence.created"

	// EventCadenceUpdated — an Archon changed a Cadence (recipe / schedule /
	// enabled toggle) via `PATCH /v1/cadences/{id}` or an MCP tool. `source:
	// api`/`mcp`, `archon_aid` is the initiator. Payload: `{cadence_id, name,
	// schedule_kind, kind, overlap_policy, enabled}` (fields after the edit;
	// `input` is not included).
	EventCadenceUpdated EventType = "cadence.updated"

	// EventCadenceDeleted — an Archon deleted a Cadence via
	// `DELETE /v1/cadences/{id}` or an MCP tool. `source: api`/`mcp`,
	// `archon_aid` is the initiator. Spawned Voyages remain (FK
	// `voyages.cadence_id` ON DELETE SET NULL, ADR-046 §9). Payload:
	// `{cadence_id}`.
	EventCadenceDeleted EventType = "cadence.deleted"

	// EventCadenceSpawned — the Reaper leader, on a `spawn_due_cadence` tick,
	// spawned a child Voyage from a Cadence recipe (ADR-046 §8): a due schedule
	// (enabled AND next_run_at <= NOW()) with a permitting overlap_policy →
	// Insert voyages/voyage_targets with a cadence_id back-link, in one spawn tx
	// with advancing next_run_at/last_run_at. Area `cadence.*` (a control entity
	// — keeper-side). `source: background` (a background periodic Reaper rule,
	// parity with `scry_background`; NB: ADR-046 §8 / naming-rules.md mention
	// `scheduler` — that value is NOT in the closed audit.Source enum, see
	// observations), `archon_aid` = the Cadence's `created_by_aid` (spawn on
	// behalf of the creator, ADR-046 §7), `correlation_id` = voyage_id. Payload:
	// `{cadence_id, voyage_id, scheduled_for, scope_size}` — `scheduled_for` is
	// the planned moment (next_run_at before recompute); the recipe's `input`
	// is NOT put in the payload (invariant A ADR-027).
	EventCadenceSpawned EventType = "cadence.spawned"

	// EventCadenceSkippedOverlap — `overlap_policy: skip` skipped a spawn due to
	// a live previous child (ADR-046 §5/§8): next_run_at arrived, but the
	// Cadence has a non-terminal (pending/scheduled/running) child Voyage → the
	// spawn does NOT happen, next_run_at is recomputed anyway (the series does
	// not "stick"). `source: background`, `archon_aid` = `created_by_aid`,
	// `correlation_id` = cadence_id (no voyage_id — there was no spawn).
	// Payload: `{cadence_id, scheduled_for, reason: "overlap"}`.
	EventCadenceSkippedOverlap EventType = "cadence.skipped_overlap"

	// EventHeraldDelivered — the terminal of a SUCCESSFUL notification delivery
	// to a Herald channel (ADR-052(d), S3): the claim-queue worker did a webhook
	// POST, the endpoint returned 2xx. at-least-once — the statuses of
	// in-flight attempts live in Redis (hot→Redis, ADR-006); only the terminal
	// is written to audit (a permanent auditable trail). `source:
	// keeper_internal` (worker-initiated, not an operator), `archon_aid: NULL`,
	// `correlation_id` = the run event's correlation_id (voyage_id/apply_id).
	// Payload: `{herald, tiding, event_type, attempt, status_code}` — the
	// notification payload values are NOT duplicated in audit (invariant A
	// ADR-027 — they may carry vault-resolved data).
	EventHeraldDelivered EventType = "herald.delivered"

	// EventHeraldFailed — the terminal of a FAILED notification delivery
	// (ADR-052(d), S3): retry-backoff exhausted / SSRF guard rejected the URL /
	// endpoint unreachable / non-2xx after all attempts. `source:
	// keeper_internal`, `archon_aid: NULL`, `correlation_id` = the run event's
	// correlation_id. Payload: `{herald, tiding, event_type, attempt,
	// error_message}` — `error_message` is masked (MaskSecrets: the cause may
	// transit a vault-ref). The notification payload values are NOT duplicated
	// in audit (invariant A ADR-027).
	EventHeraldFailed EventType = "herald.failed"

	// EventHeraldCreated / EventHeraldUpdated / EventHeraldDeleted — the CRUD
	// family of the Herald-channel registry (ADR-052(f), S4): operator-initiated
	// CRUD via POST/PUT/DELETE /v1/heralds* (and the mirror MCP
	// keeper.herald.*). `source` = api/mcp, `archon_aid` = JWT.sub. Payload:
	// `{name, type, url, secret_ref, created_by_aid}` — `url` (not a secret for
	// a webhook) and `secret_ref` (a vault-ref, not the secret itself) are
	// written; the channel's secret is not in the record. Delete cascade-removes
	// the related Tidings (ON DELETE CASCADE).
	EventHeraldCreated EventType = "herald.created"
	EventHeraldUpdated EventType = "herald.updated"
	EventHeraldDeleted EventType = "herald.deleted"

	// EventTidingCreated / EventTidingUpdated / EventTidingDeleted — the CRUD
	// family of the Tiding subscription-rule registry (ADR-052(f), S4):
	// operator-initiated CRUD via POST/PUT/DELETE /v1/tidings* (and the mirror
	// MCP keeper.tiding.*). `source` = api/mcp, `archon_aid` = JWT.sub. Payload:
	// `{name, herald, event_types, only_failures, only_changes, incarnation,
	// cadence, created_by_aid}` — all values are public (area-glob lists /
	// names, not secrets).
	EventTidingCreated EventType = "tiding.created"
	EventTidingUpdated EventType = "tiding.updated"
	EventTidingDeleted EventType = "tiding.deleted"

	// EventProvisioningPolicyChanged — an Archon changed the policy of operator
	// CREATION methods (`provisioning_allowed_methods` in keeper_settings,
	// ADR-058 Part B) via `PUT /v1/provisioning-policy`. `source: api`,
	// `archon_aid` is the initiator. Payload: `{allowed_methods, previous?}` —
	// `allowed_methods` (the new list from {user,ldap,oidc}) and `previous` (the
	// prior list, if the policy was set) are not a secret and are logged. A
	// security-sensitive mutation (it governs access to operator provisioning) —
	// must be audited.
	EventProvisioningPolicyChanged EventType = "provisioning.policy_changed"

	// EventOperatorLogin — an operator successfully passed federated
	// authentication (LDAP search-bind, ADR-058) and obtained an internal JWT
	// via `POST /auth/ldap/login`. Written by the endpoint AFTER the JWT is
	// issued (one event per successful login). `source: api`, `archon_aid` = the
	// authenticated AID. Payload: `{method, aid, provisioned}` — `method` ∈
	// `ldap` (OIDC is stage 2); `provisioned` = true if this login
	// auto-provisioned a new operator. The password / bind-creds / group secrets
	// are NOT put in the payload (security hygiene).
	EventOperatorLogin EventType = "operator.login"

	// EventOperatorProvisioned — auto-provision of a new Archon on the first
	// federated login (ADR-058): an external identity in a group from
	// group_role_map → insert of an `operators` row with `auth_method=ldap` and
	// roles from the groups. Written by the Mapper when the row is created (one
	// event per provision; the login is recorded by a separate
	// `operator.login`). `source: api`, `archon_aid` = the new AID. Payload:
	// `{aid, auth_method, display_name, roles, groups}` — roles/groups are not a
	// secret; the password / bind-creds are NOT put in the payload.
	EventOperatorProvisioned EventType = "operator.provisioned"

	// EventProviderCreated — an Archon created a Cloud Provider (the `providers`
	// registry, ADR-017, docs/keeper/cloud.md) via the Operator API
	// (`POST /v1/providers`) or the MCP tool `keeper.provider.create`. `source:
	// api`/`mcp`, `archon_aid` is the initiator. Payload: `{name, type, region,
	// credentials_ref}` — all fields cleartext infrastructure:
	// `credentials_ref` is a PATH (`vault:<path>`), not the secret itself
	// (security hygiene like the jwt-signing-key-ref); the real creds are NOT
	// resolved and NOT written into audit.
	EventProviderCreated EventType = "provider.created"

	// EventProviderDeleted — an Archon deleted a Cloud Provider via
	// `DELETE /v1/providers/{name}` or the MCP tool `keeper.provider.delete`.
	// `source: api`/`mcp`, `archon_aid` is the initiator. Payload: `{name}`.
	EventProviderDeleted EventType = "provider.deleted"

	// EventProfileCreated — an Archon created a Cloud Profile (a VM-spec on top
	// of a Provider, the `profiles` registry, ADR-017) via `POST /v1/profiles`
	// or the MCP tool `keeper.profile.create`. `source: api`/`mcp`, `archon_aid`
	// is the initiator. Payload: `{name, provider, params_keys}` — the VALUE
	// params are NOT put in audit (keys only, symmetric with push-provider): a
	// freeform VM-spec may carry sensitive values.
	EventProfileCreated EventType = "profile.created"

	// EventProfileDeleted — an Archon deleted a Cloud Profile via
	// `DELETE /v1/profiles/{name}` or the MCP tool `keeper.profile.delete`.
	// `source: api`/`mcp`, `archon_aid` is the initiator. Payload: `{name}`.
	EventProfileDeleted EventType = "profile.deleted"
)
