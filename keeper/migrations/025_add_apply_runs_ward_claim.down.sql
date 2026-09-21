-- 025_add_apply_runs_ward_claim.down.sql
--
-- Revert of ADR-027 Phase 0. status is a CHECK constraint (NOT a PG enum), so
-- extending it with planned/claimed values is FULLY reversible: we revert
-- the constraint to the 018+024 form (running/success/failed/cancelled). The caveat about
-- the irreversibility of `ALTER TYPE ... ADD VALUE` does NOT apply here - there is no enum.
--
-- Precondition for down: apply_runs must not have any remaining rows with status in
-- ('planned','claimed') - otherwise recreating the CHECK will fail (23514). In Phase 0 these
-- values are never written by anyone, so on a clean Phase 0 the revert is safe.

DROP INDEX IF EXISTS apply_runs_claim_scan_idx;

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('running', 'success', 'failed', 'cancelled'));

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS attempt,
    DROP COLUMN IF EXISTS claim_expires_at,
    DROP COLUMN IF EXISTS claim_at,
    DROP COLUMN IF EXISTS claim_by_kid;
