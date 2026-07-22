# Upgrade procedure

Rolling upgrade of Keeper cluster and Soul fleet, state_schema migration, rollback, compatibility.

## Principles

| Principle | Where recorded |
|---|---|
| **Forward-compat only-add** in proto Keeper↔Soul ([ADR-012(g)](../adr/0012-keeper-soul-grpc.md)) - never delete fields or reuse field numbers. Breaking changes only via `proto/keeper/v2/`. | [ADR-012](../adr/0012-keeper-soul-grpc.md) |
| **Forward-compat only-add** in Operator API (REST + MCP) - inside `/v1/` only-add. Breaking - `/v2/`. | [`docs/keeper/operator-api.md` → conventions](../keeper/operator-api.md#conventions) |
| **State_schema migrations forward-only** in MVP - `down:` is not supported. Rollback - via `state_history` snapshot. | [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) |
| **Hot-reload config** ([ADR-021](../adr/0021-hot-reload-config.md)) - most of `keeper.yml` reload-able via SIGHUP. The list of restart-only fields is in [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload). | [ADR-021](../adr/0021-hot-reload-config.md) |
| **Compatibility of Keeper N ↔ Soul N-1**: the new Keeper understands the proto-messages of the old Soul; the new Soul understands the proto-messages of the old Keeper (forward-compat); add-only fields - null / `0` means "old version, no feature". | [ADR-012(g)](../adr/0012-keeper-soul-grpc.md) |

## Rolling upgrade Keeper

Multi-keeper cluster allows you to update instances one at a time, without downtime.

### Procedure

1. **Pre-flight checks:**
   - Backup PG before upgrade (see [`infra.md` → Backup](infra.md#backup--restore)).
   - Check that there are no breaking-format migrations (changelog between versions).
   - Verify cluster health: `keeper_reaper_lease_held` (sum=1), `keeper_grpc_streams_active` is stable.

2. **Upgrade of the first instance:**
   ```sh
   ssh keeper-1.internal
   # Drain LB
   # ... output from active backend to LB
   # Install new version
   sudo dpkg -i /tmp/soul-stack-keeper_<new-version>_amd64.deb
   # systemd Restart=on-failure will rise with the new version
   systemctl restart keeper
   # Verify started
   journalctl -u keeper -n 100 --no-pager
   ```

**What happens** during a restart:
   - graceful shutdown of the current version: Acolyte-drain (`acolyte_drain_grace`), Conclave-snap, EventStream streams are closed → Souls failback for the remaining instances.
   - start of new version: `state_schema` migrations are applied (see [§ State_schema migrations](#state_schema-migrations)), Conclave-presence is written anew.

3. **Verify** new instance:
   - `redis-cli KEYS 'keeper:instance:*'` — N keys.
   - `keeper_grpc_streams_active` starts to grow as Souls failback.
   - `keeper_rbac_snapshot_rebuild_errors_total` is not growing.
   - `/readyz` HTTP probe returns 200.

4. **Return to LB**, move to next instance. There is a 30s-1min pause between instances so that Souls have time to re-distribute.

5. **Final check**: after updating all instances - `keeper --version` on each, `keeper_grpc_streams_active` in total is equal to the number of connected Souls.

### Rollback on failure

If the new version does not start / crashes according to `keeper_*` metrics:

1. **Do not restart with a new one**, do not panic - the old instances continue to be served.
2. Rollback package: `sudo dpkg -i /tmp/soul-stack-keeper_<previous-version>_amd64.deb && systemctl restart keeper`.
3. Verify - startup logs without errors, Conclave-presence has arrived.
4. Investigate the new version separately.

**If the state_schema migration has already been processed** when a new version is upgraded, rolling back the binary **does not roll back the migration** (forward-only). The old version may not understand the new `incarnation.state` schema. Recovery - see § State_schema rollback.

## State_schema migrations

`state_schema` - versionable incarnation runtime data schema ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). The version is bumped into `service.yml::state_schema_version`; migrations live in the service repo under `migrations/<NNN>_to_<MMM>/`.

### When to use

When **processing `incarnation.upgrade`** through the Operator API - operator-initiated, **not lazy** at the start of Keeper. Atomically with one PG transaction: `SELECT FOR UPDATE → in-memory in-Go application → per-step snapshot into state_history → COMMIT` ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

If there is a problem - migration rollbacks the transaction, `incarnation.status` goes to `migration_failed` (terminal; remediation = rollback to the target version or fix migration + replay).

### What to backup before `incarnation.upgrade`

Snapshot `state_history` before version bump **never archived** by Reaper rule `archive_state_history` with `keep_version_bump_snapshots: true` (default) - restorable anchor.

Additionally - full PG-backup before mass migration (if you plan to upgrade N incarnations at once).

### Rollback state_schema

`down:` is not supported in DSL ([ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). Recovery after unsuccessful migration:

1. **`incarnation.status = migration_failed`** — incarnation locked.
2. **Restore state from snapshot:**
   ```sql
-- find snapshot before migration (the one that is not archived)
   SELECT at, scenario, state_before, state_after
   FROM state_history
   WHERE incarnation_name = '<name>' AND scenario = 'migration'
   ORDER BY at DESC LIMIT 5;

-- restore state to incarnation
   UPDATE incarnation
   SET state = (SELECT state_before FROM state_history WHERE at = '<chosen-snapshot-time>'),
       state_schema_version = <previous-version>,
       status = 'ready'
   WHERE name = '<name>';
   ```
3. **Commit migration** to service repo, release new service ref.
4. **Repeat `incarnation.upgrade`** with fixed migration.

This operation is **emergency** and requires direct access to the PG. An alternative is to leave the incarnation in `migration_failed` and recreate it (if the migration was the first and the stake is not critical).

## Rolling upgrade Soul fleet

Soul agents are updated independently of Keeper, because forward-compat works **both ways** ([ADR-012(g)](../adr/0012-keeper-soul-grpc.md)):

- The new Keeper accepts proto-messages from the old Soul - missing only-add fields = `0`/`nil` (forward-compat degradation: for example, `WardRoster` from the old Soul = empty → sweep no-op fail-safe, see [ADR-027(g)](../adr/0027-apply-work-queue.md) amend).
- The new Soul receives proto-messages from the old Keeper - the same thing (`ApplyRequest.attempt = 0` → fencing degrades to protection according to `apply_id`, see [ADR-027(g)](../adr/0027-apply-work-queue.md)).

### Procedure (via Destiny)

The canonical way to upgrade the Soul fleet is the Soul Stack itself:

```yaml
# destiny/soul-upgrade/destiny.yml
input:
  to_version: { type: string, required: true }

tasks:
  - name: install-new-binary
    module: core.pkg
    state: installed
    name: soul-stack-soul
    version: "${ input.to_version }"
  - name: restart-soul
    module: core.service
    state: restarted
    name: soul
```

Roll out via `keeper.incarnation.run` to coven of managed hosts.

### Procedure (manual)

```sh
# on every Soul host
sudo dpkg -i /tmp/soul-stack-soul_<new-version>_amd64.deb
systemctl restart soul
```

After Soul restart:
- Bootstrap is NOT happening again - SoulSeed is already released, mTLS session is restored.
- Soul reconnects to any Keeper from the priority list.
- `WardRoster`-snapshot is sent upon reconnect (S6 Soul-reconcile, [ADR-027(g)](../adr/0027-apply-work-queue.md)) — Keeper checks dispatch applications.

**Soul version** is injected during build (`SOUL_LDFLAGS := -X .../soul.soulVersion=$(VERSION)`, [`Makefile`](../../Makefile)) and goes to `Hello`/`BootstrapRequest` for auditing.

## `state_schema` migrations - operational workflow

Full migration spec - [`docs/migrations.md`](../migrations.md). Cycle for operator:

1. **Service developer** edits `service.yml::state_schema_version` (bump) + puts `migrations/<N>_to_<M>/migration.yml` with DSL operations (`rename`/`set`/`delete`/`move`, optional `foreach` for collections) + tests `migrations/<N>_to_<M>/tests/<case>.yml`.
2. **CI** runs `soul-trial` ([ADR-023](../adr/0023-trial-test-runner.md)) - migration is applied on state-fixtures, assertion `state_after`.
3. **Service-repo** is merged, new git-ref is released ([ADR-007](../adr/0007-versioning-git-ref.md)).
4. **Operator** updates `service_registry.ref` via Operator API: `POST /v1/services/{name}` with new `ref:`.
5. **Operator** runs `incarnation.upgrade` via the Operator API on a specific incarnation: `POST /v1/incarnations/{name}/upgrade`. Atomic single PG transaction (see above).
6. **Verify**: `GET /v1/incarnations/{name}` → `status: ready`, `state_schema_version: <M>`. Migration history - to `state_history` from `scenario: migration`.

If the problem is `status: migration_failed`, see § Rollback state_schema.

## Version compatibility

### Keeper version N ↔ Soul version M

| Script | Behavior |
|---|---|
| Keeper N, Soul N | All features are available. |
| Keeper N+1 (new), Soul N (old) | OK. `ApplyRequest.attempt` is written, but Soul ignores it / does not fencing. `WardRoster` from Soul is empty → orphan-sweep no-op. |
| Keeper N (old), Soul N+1 (new) | OK. Soul sends `WardRoster`, Keeper does not understand it (silently ignores unknown-oneof). `RunResult.attempt = 0` — Keeper does not perform an epoch-check at the reception. |

**Major-bump** (`/v1/` → `/v2/`) is the only case of breaking. At the time of writing there are no plans; when it appears, a separate procedure with side-by-side release `proto/keeper/v2/`.

### Keeper version N ↔ Operator API clients

| Script | Behavior |
|---|---|
| Old client (REST/MCP) | OK. New fields in the response are ignored (JSON unknown-field-tolerant), the client simply does not call new endpoints. |
| New client vs old Keeper | OK for existing endpoints. New endpoints (introduced in the new version of Keeper) - 404 on the old one → graceful-fallback client. |

## Backup before major operations

Checklist before any non-standard operation (mass upgrade, migration, key rotation):

- [ ] PG backup (logical or physical PITR).
- [ ] Vault snapshot (raft snapshot).
- [ ] Configs `keeper.yml` / `soul.yml` of all instances - in git, committed.
- [ ] mTLS material and TLS certificates are recorded in the Vault PKI / in the operator's secret manager.
- [ ] Audit-log - last upload to the cold archive (for post-incident-debugging).

## See also

- [`docs/architecture.md` → ADR-012 / ADR-019 / ADR-021](../architecture.md) - rationale for forward-compat and hot-reload.
- [`docs/migrations.md`](../migrations.md) - regulatory spec for state_schema migrations.
- [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload) — per-block hot-reload policy.
- [`disaster-recovery.md`](disaster-recovery.md) - recovery in case of migration failure / failure after upgrade.
