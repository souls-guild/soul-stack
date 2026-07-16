# Enabling `reclaim_apply_runs` in production

Operationalization of GATE-1 (deliver-once recovery) from [ADR-027](../adr/0027-apply-work-queue.md). Not a separate ADR - switching an already implemented rule, according to the list of product gates.

**What it does.** Reaper rule `reclaim_apply_runs` ([keeper/internal/reaper/runner.go](../../keeper/internal/reaper/runner.go)) rebrands stuck `apply_runs.status='claimed'` back to `planned` if `claim_expires_at < NOW()`. Closes the "hanging applying" hole in the **before recoil** phase (Keeper died on render/claim, Soul's task has not yet left): the living Acolyte will pick up the line with the increment `attempt`, and Soul-side fencing will cut off the outdated take of the previous owner. Phase **`dispatched`** (after the return) is **not affected by the rule** - re-claim what has already been given = double apply.

Without the rule, a Keeper instance that crashes in the `claimed` phase leaves `apply_run` stuck forever - the operator must remove it manually.

**By default `enabled: false`** (ADR-027 amend (e), [docs/keeper/reaper.md → Config](../keeper/reaper.md)). Turning on is an operational step under the gate, not a single `enabled: true`.

## Product gates (all three are required)

### 1. Fencing-Soul rolled out to the entire fleet

ADR-027 amend GATE-1 + (g) + (e). All `soul` agents must carry attempt-fencing: Soul-guard by `ApplyRequest.attempt` at execution + echo `RunResult.attempt` for epoch-check at reception. Without this re-claim, the stale Ward will send a second `ApplyRequest` to the host, and the non-fenced Soul will not cut off the stale take.

**The current `soul` build carries fencing** (gate-1 attempt-fencing is already in the binary, [ADR-027](../adr/0027-apply-work-queue.md) amend (b)/(g) is implemented). It is enough to make sure that this build is rolled out **to the entire park** (and not a legacy build without an attempt field).

**Check:** under the flood test of outdated `RunResult` metric `keeper_runresult_stale_total` is consistently non-zero - epoch-check cuts off stale-`RunResult` at reception. If 0 in obvious stale scenarios, fencing is not rolled out everywhere.

### 2. `acolytes > 0` on ALL Keeper instances

ADR-027 amend (f) + (e). `reclaim_apply_runs` and `acolytes > 0` are a **related pair**. With `acolytes: 0`, jobs are executed in the old synchronous way and written directly to `running`; reclaim on a non-fenced path without epoch protection will double apply.

**Check:** on each keeper node of the cluster `keeper.yml::acolyte.workers > 0` (see [config.md → acolytes](../keeper/config.md#acolytes)). Through [Conclave](../adr/0006-cache-redis.md): refuse-startup-guard already refuses to start in the dangerous `acolytes:0 + CountLive>1` setup ([ADR-027(h)/(k)](../adr/0027-apply-work-queue.md)), but the operator still confirms explicitly.

### 3. Soul-reconcile (S6) active for `dispatched` orphans

ADR-027 amend (g), implemented 2026-05-25 - `WardRoster` ([ADR-012(k)](../adr/0012-keeper-soul-grpc.md)). Closes the window "Keeper and Soul are both dead after recoil": Soul on (re)connect sends `WardRoster` (ReplaceAll-snapshot of slaves `apply_id`+attempt), Keeper in `OrphanDispatched` epoch-fenced single-winner-reconciliation terminalit `dispatched`-lines of this SID, which are not in the set → new terminal `orphaned` (migration 044).

Without this, `dispatched` lines in the fleet with killed Keeper+Soul pairs would hang forever - `reclaim` does not touch them by design.

**Check:** alert **dispatched stuck (post-recovery-enable)** ([monitoring.md](monitoring.md)) - SQL `COUNT(*) FROM apply_runs WHERE status = 'dispatched' AND claim_at < NOW() - INTERVAL '1 hour'` should remain around 0 on a stand with a simulated kill keeper+soul. If it saves, Soul-reconcile does not work (old `soul` build without `WardRoster`, forward-compat no-op fail-safe).

### Without gates - DO NOT enable

- Without step 1 (fencing-Soul) → race-condition "two Keepers simultaneously roll out one apply_run to the host."
- Without step 2 (`acolytes > 0`) → reclaim returns to `planned`, but no one will pick it up; on a non-fenced path, reclaim is generally unsafe.
- Without step 3 (`WardRoster`) → `dispatched`-orphans save indefinitely.

## Enable

In `keeper.yml`:

```yaml
reaper:
  interval: 30s              # tick-Reaper-cycle frequency; recommended <= acolyte.claim_lease / 2
  rules:
    reclaim_apply_runs:
      enabled: true          # default false
      stale_after: 1m        # formal lease argument of the action scheme; NOT included in the SQL predicate
      action: set_status
      target_status: planned
```

`acolyte_lease > max-render` — default `30s` ok (lease invariant **weakened** after GATE-1, [ADR-027 amend (e)](../adr/0027-apply-work-queue.md)). `acolyte_lease > max(single-host apply time)` and Acolyte lease-renew - **no longer required** for the recovery boundary.

Apply via hot-reload (`SIGHUP` or API/MCP config mutation, [config.md → Hot-reload](../keeper/config.md#hot-reload)) - without restarting the keeper process.

## Validation after enable

1. **Reaper rule metrics** ([reaper.md → Metrics](../keeper/reaper.md)):
   - `keeper_reaper_rule_executions_total{rule="reclaim_apply_runs"}` — grows by the tick of the Reaper cycle (the rule is called). It's "how many times it was called", not "how many times it worked".
   - `keeper_reaper_rule_purged_total{rule="reclaim_apply_runs"}` — total number of advertised rows (set_status → planned). Under load with simulated kill-Keeper during `claimed` - it grows. On a stable cluster without kill, it should be about 0.
   - `keeper_reaper_dispatch_errors_total{rule="reclaim_apply_runs"}` - should be 0. Growth = PG failure on reclaim.

2. **Audit-events** ([naming-rules.md → Audit-events](../naming-rules.md#audit-events), area `reaper.*`):
   - `reaper.reclaim_apply_runs.executed` - written when triggered. SQL: `SELECT * FROM audit_log WHERE event_type = 'reaper.reclaim_apply_runs.executed' ORDER BY created_at DESC LIMIT 20`.

3. **Epoch-check at reception** ([monitoring.md](monitoring.md)):
   - `keeper_runresult_stale_total` - spikes when re-claiming the old Ward (Soul sent the outdated `RunResult` with the old `attempt`). This is **expected** and means fencing is working.
   - **Linear increase in the number of repeated apply on the same hosts without stale cutoff** = alarm: fencing is not rolled out to the entire fleet, return to gate 1.

4. **Alert `dispatched stuck`** — Soul-reconcile should orphan `dispatched`-lines after the death of the Soul-owner ([monitoring.md → Warning](monitoring.md)). Growth - Soul-reconcile is not active.

## Rollback

```yaml
reaper:
  rules:
    reclaim_apply_runs:
      enabled: false
```

Hot-reload. Safe at any time - the rule is idempotent (only status-update `claimed → planned`, not data-mutation, not deletion). After rolling back the `claimed` rows of the rotten owner are stuck again while the rule is disabled.

## Voyage-orphan-lock-release - the same double-apply class, enabled differently

This runbook is about `reclaim_apply_runs` (per-host Ward, default-OFF, gate above). Nearby there lives a **second recovery mechanism** with the same acceptable double-apply class, but **enabled in a fundamentally different way** - the operator must understand the difference.

**What is this.** `reclaim_voyages` ([ADR-043 §8](../adr/0043-voyage.md), [reaper.md → Config](../keeper/reaper.md)) returns the orphaned `running`-Voyage (owner-Keeper died before finalization, lease is rotten) back to `pending`; another Keeper instance reclaims it and completes it from the saved `current_batch_index`. For `kind=scenario`-Voyage, when re-running a leg, the branded VoyageWorker encounters the orphaned `incarnation.status='applying'`, left by the crashed previous owner (single-winner state-commit `applying`→the terminal did not work for him - he died). **Voyage-orphan-lock-release** ([ADR-027(l)](../adr/0027-apply-work-queue.md)) - the seam that VoyageWorker **removes ITS orphaned `applying`** before re-run. Without it, reclaimed Voyage freezes ("incarnation is already applying"), and `reclaim_voyages` itself turns out to be inoperative.

**Enabled differently than `reclaim_apply_runs`:**

| | `reclaim_apply_runs` (this runbook) | Voyage-orphan-lock-release |
|---|---|---|
| Default | **OFF** (map-driven, needs explicit `enabled: true`) | **ON always** - built-in re-run path `reclaim_voyages`, no separate switch |
| Gate before switching on | yes, three product gates above | no - comes with `reclaim_voyages` (the same default-ON, path-defaulting) |
| Who included on the prod | operator deliberately, under the gate | **already enabled by default** |

`reclaim_voyages` default-ON deliberately (target scale 100k Souls, restart/replacement of Keeper instance is a regular event, orphaned Voyages are regular → recovery should not depend on manually writing the rule). And since Voyage-recovery is broken without orphan-lock-release, the seam does not have its own opt-in.

**The same double-apply class.** With network-partition (the living but partitioned previous owner continues to apply while our re-run sends the second one) the host can receive **double job submission** - exactly like `reclaim_apply_runs`. **damage to `incarnation.state`** is protected by the same two barriers that are described in this runbook for per-host reclaim:

1. **gate-1 attempt-fencing `RunResult`** — stale-`RunResult` of the previous owner is cut off at the reception by `apply_runs.attempt` (epoch increased during re-claim), a surge of `keeper_runresult_stale_total` (see Validation → Epoch-check) with failover Voyage is expected and means fencing is working;
2. **idempotency of modules** - resending the same job to the host does not change the result if the module is written correctly (same recommendation as for command-Voyage in [ADR-043 §8(d)](../adr/0043-voyage.md)).

**Practical conclusion to the operator.** This double-apply class is present in the product **regardless of whether you have enabled `reclaim_apply_runs`** - it comes along with default-ON `reclaim_voyages`. Rolling out fencing-Soul (gate 1 above) and idempotency of modules is a condition for the correctness of **both** mechanisms, not only per-host reclaim. If `reclaim_voyages` is disabled for some reason by an explicit `reaper.rules.reclaim_voyages.enabled: false` - orphan-lock-release goes away with it, but then orphaned Voyages hang without recovery (undesirable on production).

Regulatory lock - [reaper.md → Voyage-orphan-lock-release block](../keeper/reaper.md) and [ADR-027(l)](../adr/0027-apply-work-queue.md).

## standalone-orphan reconcile - the same class for direct run

Voyage-orphan-lock-release (above) closes the orphaned `applying` for **Voyage runs only** - they have a backlink `voyage_targets.apply_id`. **Direct (standalone) `incarnation.run`** - running the script without the Voyage batch wrapper - the line `voyage_targets` does not have, so the crash of its Keeper owner would leave `incarnation.status='applying'` forever. This seam is closed by the Reaper rule **`reconcile_orphan_applying`** ([ADR-027(m)](../adr/0027-apply-work-queue.md), recovery-completeness backstop).

**What it does.** Removes the orphaned direct applying-lock: `applying → ready`, after which incarnation is launched again. Two-phase detection - (1) SQL candidates: stale applying lines (`applying_since` older than `stale_after`, 90s by default) with a NOT empty epoch (`applying_by_kid`); (2) presence check: whether `applying_by_kid` is alive in Conclave (`InstanceAlive`). Lock is removed **only** if the owner is proven dead (presence=false); the live owner (the run is actually running) is skipped, the presence check error (Redis flap) is fail-safe skip (the live run does not fail).

**default-ON, like `reclaim_voyages`** (path-defaulting): works if there is no key in `reaper.rules`, disabled only by explicit `reconcile_orphan_applying.enabled: false`. It does not have a separate product gate (unlike `reclaim_apply_runs`).

**How it differs from neighboring recovery rules:**

| Rule | What does it advertise | Default |
|---|---|---|
| `reclaim_apply_runs` | rancid `claimed`-Ward in `apply_runs` (phase before sending to host) | **OFF**, three food gates |
| `reclaim_voyages` (+ Voyage-orphan-lock-release) | orphaned `running`-Voyage → `pending`; advertised worker removes `applying` under-Voyage | **ON** (path-defaulting) |
| `reconcile_orphan_applying` | orphaned `applying`-lock **direct** `incarnation.run` (outside Voyage) → `ready` | **ON** (path-defaulting) |

**Known-gap - NULL-epoch + FromLocked-microwindow → manual `unlock`.** The rule advertises only lines with a known epoch (`applying_by_kid IS NOT NULL`). Two classes are left behind:

- **legacy/pre-082** — applying lines supplied before migration 082 (there were no epoch columns yet) carry NULL `applying_by_kid`;
- **rerun-last microwindow** — `UnlockForRerun` transit `error_locked → applying` WITHOUT epoch, epoch is appended to the next tx; The crash exactly in the gap between these two tx leaves a NULL-epoch.

Without a presence witness to the death of the owner (no `applying_by_kid`), withdrawal is unsafe - such a lock rule is deliberately NOT touched. Removed manually by the operator: `POST /v1/incarnations/{name}/unlock` ([operator-api/incarnations.md → unlock](../keeper/operator-api/incarnations.md)) after analyzing that the run is really dead. Diagnostics `applying`-stuck - [faq.md](faq.md).

**Residual double-apply class is the same as `reclaim_apply_runs`.** With network-partition (a live, but partitioned owner continues to apply while reconcile has removed the lock and incarnation has been restarted) a second run may take the host. The same two barriers protect `incarnation.state` from corruption: gate-1 attempt-fencing `RunResult` + module idempotency (see Voyage-orphan-lock-release above). Rolling out fencing-Soul (gate 1) is a condition for the correctness of this mechanism.

Regulatory fixation - [reaper.md → `reconcile_orphan_applying`](../keeper/reaper.md) and [ADR-027(m)](../adr/0027-apply-work-queue.md).

## presence-gated force-release SID-lease - shortening Soul's invisibility window

Separate recovery-backstop ([ADR-027(n)](../adr/0027-apply-work-queue.md), S2) - **not a Reaper rule**, but a seam in the EventStream handler of Soul's reconnect. This does not apply to the applying-lock of the incarnation, but to the SID-lease of the stream (`soul:<sid>:lock`).

**Problem.** Soul reconnects to **another** Keeper instance after the death of the previous holder. The SID-lease of the previous holder still holds (TTL **60s**), and without a seam the new Keeper would give Soul to `AlreadyExists` - Soul would retrail until the TTL expires, remaining **invisible** to the fleet this entire window (up to 60s).

**What the seam does.** Instead of failure, the handler presence-gated intercepts the lease from the **proven-dead** prev-holder: checks `InstanceAlive(prev_kid)` in Conclave (presence key of the keeper instance, TTL **30s**); if prev-holder is dead (`InstanceAlive=false`) - CAS-by-prev-holder `ForceAcquireSoulLease` recaptures the key to a new KID. The invisibility window is reduced from **60s** (wait for TTL SID-lease) to **≤30s** (TTL Conclave-presence - until the previous holder falls off the Conclave, he is considered alive). Security-event `eventstream.lease_force_released {sid, prev_kid, new_kid}` (`source: soul_grpc`) - for each successful interception.

**Split-brain security via presence-gate.** Interception occurs **only** when `InstanceAlive=false` (prev-holder is proven dead). If the prev-holder is alive, the presence check has fallen (Redis flap), prev-holder == self (reconnect to the same keeper) or the key has changed to a third one between the check and CAS - the seam **doesn't** intercept, gives Soul to `AlreadyExists` (that retrait). These refusals are NOT audited - this is a standard "give Soul a retrain", not an incident. Therefore, two living Keepers cannot simultaneously hold one SID-lease: exactly one gets ownership through the presence-proof of the death of the other.

**Residual - the window ≤Conclave-TTL is not zero.** The seam reduces the invisibility window, but does not reset it to zero: the prev-holder remains "alive" in the Conclave until its presence-TTL expires (30s), and until that point the new Keeper honestly gives `AlreadyExists`. This is by-design - it is impossible to prove death earlier without the risk of split-brain.

**Soul-side is complementary.** While the keeper gives `AlreadyExists` (lease is still holding), Soul distinguishes this lease-held soft-failure from a transport failure and retraits with a modest-backoff-cap (3s), and not a common transport-cap (30s) - in order to reconnect within seconds after the force-release, and not to hammer the surviving keepers the entire presence window. See [docs/soul/connection.md → Lease-held soft-failure](../soul/connection.md).

Regulatory fixation - [naming-rules.md → `eventstream.lease_force_released`](../naming-rules.md#audit-events) and [ADR-027(n)](../adr/0027-apply-work-queue.md).

## Related

- [ADR-027 - Acolyte / Ward / recovery](../adr/0027-apply-work-queue.md), amend GATE-1 (deliver-once recovery, 2026-05-25); (l) - Voyage-orphan-lock-release.
- [ADR-043 §8 - Voyage failover / `reclaim_voyages` default-ON](../adr/0043-voyage.md).
- [docs/keeper/reaper.md → Enabling recovery](../keeper/reaper.md) - regulatory gate fixation, source of truth.
- [docs/keeper/reaper.md → Metrics](../keeper/reaper.md) - canonical names of Reaper metrics.
- Parallel Reaper rules that **do not block** enabling this: `mark_disconnected` (Soul heartbeat drop, 90s), `reconcile_orphan_applying` (standalone-orphan applying-lock, default-ON, see section above), `purge_audit_old`, `purge_apply_runs` (30d retention completed), `purge_apply_task_register` (1h grace), `purge_old_errands`.
- [ADR-027(m)/(n)](../adr/0027-apply-work-queue.md) — recovery-completeness backstop: standalone-orphan reconcile (`reconcile_orphan_applying`) + presence-gated force-release SID-lease (`eventstream.lease_force_released`).
- [monitoring.md](monitoring.md) — alert `dispatched stuck (post-recovery-enable)`.
- [faq.md](faq.md) - diagnostics `applying`-stuck.
