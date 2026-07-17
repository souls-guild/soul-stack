-- 025_add_apply_runs_ward_claim.up.sql
--
-- ADR-027 Phase 0: additive schema prep for `apply_runs` for the work-queue +
-- claim execution model (Ward-claim). Phase 0 is schema ONLY: execution code
-- doesn't change, the old in-memory run-goroutine path (direct `running` ->
-- terminal) keeps working unchanged. The new columns/statuses added after
-- Phase 0 are written and read by NO ONE yet (claim logic, Acolyte pool, Summons,
-- recovery scan - Phase 1+).
--
-- Ward (task sub-custody, naming-rules.md): claiming an execution task -
-- columns `claim_by_kid` / `claim_at` / `claim_expires_at` / `attempt` +
-- statuses `planned` / `claimed` on `apply_runs`. "Taking a Ward" = atomically
-- capturing a planned task (`attempt++` - fencing epoch). The target form
-- is documented in storage.md (committed together with ADR-027).
--
-- The columns are all nullable / DEFAULT, so existing apply_runs rows
-- migrate without errors (forward-only, ADR-007):
--   - claim_by_kid     - KID of the Acolyte that owns the Ward; NULL until claimed.
--   - claim_at         - moment the Ward was captured; NULL until claimed.
--   - claim_expires_at - the Ward's lease deadline; once stale (< NOW) it's returned
--                        to `planned` by the Reaper leader's recovery scan (Phase 2).
--   - attempt          - fencing epoch: incremented on every claim; 0 for rows
--                        inserted via the old path (direct `running`).
--
-- status - a CHECK constraint (NOT a PG enum, NOT bare TEXT): extended by
-- drop+recreate of the constraint (the pattern from migrations 016/017). The existing values
-- running/success/failed/cancelled (018) + cancel_requested semantics (024)
-- are PRESERVED; planned/claimed are added.

ALTER TABLE apply_runs
    ADD COLUMN claim_by_kid     TEXT,
    ADD COLUMN claim_at         TIMESTAMPTZ,
    ADD COLUMN claim_expires_at TIMESTAMPTZ,
    ADD COLUMN attempt          INTEGER NOT NULL DEFAULT 0;

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'));

-- Partial index for the claim scan / recovery scan (Phase 1+): capturing planned
-- rows (`FOR UPDATE SKIP LOCKED`) and finding stale Wards
-- (`status IN ('claimed','running') AND claim_expires_at < NOW`). Added in
-- Phase 0 - on the current path active rows are only `running` (planned/claimed
-- are written by no one), so the index is correct and harmless, and keeps the schema in its
-- target form without a separate migration in Phase 1. Terminal statuses are excluded.
CREATE INDEX apply_runs_claim_scan_idx
    ON apply_runs (status, claim_expires_at)
    WHERE status IN ('planned', 'claimed', 'running');

COMMENT ON COLUMN apply_runs.claim_by_kid IS
    'Ward-claim (ADR-027): KID of the Acolyte that owns the task; NULL until claimed. Phase 0 - not written.';
COMMENT ON COLUMN apply_runs.claim_at IS
    'Ward-claim (ADR-027): moment the task was captured; NULL until claimed. Phase 0 - not written.';
COMMENT ON COLUMN apply_runs.claim_expires_at IS
    'Ward-claim (ADR-027): lease deadline; once stale, returned to planned by the Reaper recovery scan. Phase 0 - not written.';
COMMENT ON COLUMN apply_runs.attempt IS
    'Ward-claim (ADR-027): fencing epoch, incremented on every claim. 0 for rows from the old path. Phase 0 - not incremented.';
