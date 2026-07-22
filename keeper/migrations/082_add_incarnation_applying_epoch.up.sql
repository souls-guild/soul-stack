-- 082_add_incarnation_applying_epoch.up.sql
--
-- ADR-027 amend (m), slice S0 (foundation): epoch for the incarnation's applying flag
-- for the Reaper rule reconcile_orphan_applying (standalone-orphan reconcile).
--
-- PROBLEM. A direct incarnation.run (standalone, not under a Voyage) sets
-- incarnation.status='applying' in lockRun; if the Keeper owning the run dies
-- before reaching a terminal state - the lock hangs FOREVER. The Voyage path is
-- covered by amend (l) (orphan release via the voyage_targets.apply_id back-link),
-- but a direct run has no voyage_targets row - the back-link is structurally
-- unreachable. reclaim_apply_runs doesn't reach this either (it reclaims a stale
-- claimed Ward in apply_runs, while the applying lock is a separate flag on the
-- incarnation row). Root cause: the applying flag is an ownerless bool without an
-- epoch / owner / lease.
--
-- S0 SOLUTION - SCHEMA ONLY: four NULLABLE columns give the applying flag an epoch.
-- Writing the epoch in lockRun + clearing it on terminal + the Reaper rule itself
-- are slice S1. After S0, these columns are neither written nor read by anyone.
--
--   - applying_apply_id - the apply_id of the run holding the lock; NULL while not applying.
--   - applying_attempt  - the fencing epoch of the run (parity with apply_runs.attempt).
--   - applying_by_kid   - the KID of the Keeper owning the run; a presence check in the
--                         Conclave (InstanceAlive) distinguishes "the run is in progress"
--                         from "the owner is dead, the lock is orphaned".
--   - applying_since    - the moment the lock was taken; the rule looks for stale
--                         candidates by age (applying_since < NOW() - stale_after, 90s
--                         parity with mark_disconnected).
--
-- ADDITIVITY / forward-only (ADR-007). All columns are nullable, with no backfill: for
-- existing applying rows the epoch is unknown (NULL applying_by_kid) - the rule does
-- NOT reclaim those (fail-safe, so as not to disrupt a run with an unknown epoch).

ALTER TABLE incarnation
    ADD COLUMN applying_apply_id TEXT,
    ADD COLUMN applying_attempt  INTEGER,
    ADD COLUMN applying_by_kid   TEXT,
    ADD COLUMN applying_since    TIMESTAMPTZ;

-- Partial index for the Reaper stale-candidate scan (parity with
-- apply_runs_claim_scan_idx, migration 025): the rule scans exactly the applying
-- rows by applying_since age. The WHERE predicate keeps the index narrow (only
-- applying rows - units/tens of rows per cluster), terminal/ready rows excluded.
-- Introduced in S0 - on the current path applying_since is NULL, the index is
-- correct and harmless.
CREATE INDEX incarnation_applying_scan_idx
    ON incarnation (status, applying_since)
    WHERE status = 'applying';

COMMENT ON COLUMN incarnation.applying_apply_id IS
    'standalone-orphan epoch (ADR-027(m)): apply_id of the run holding the applying lock; NULL while not applying. S0 - not written.';
COMMENT ON COLUMN incarnation.applying_attempt IS
    'standalone-orphan epoch (ADR-027(m)): fencing epoch of the run (parity with apply_runs.attempt). S0 - not written.';
COMMENT ON COLUMN incarnation.applying_by_kid IS
    'standalone-orphan epoch (ADR-027(m)): KID of the Keeper owning the run; a presence check in the Conclave (InstanceAlive) distinguishes a live run from an orphaned lock. S0 - not written.';
COMMENT ON COLUMN incarnation.applying_since IS
    'standalone-orphan epoch (ADR-027(m)): the moment the applying lock was taken; reconcile_orphan_applying looks for stale candidates by age (stale_after=90s). S0 - not written.';
