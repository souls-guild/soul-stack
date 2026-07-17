-- 040_add_apply_runs_dispatched_status.up.sql
--
-- GATE-1 recovery redesign (ADR-027 amend, S2): introduces the lifecycle
-- phase `dispatched` into the `apply_runs.status` enum. Semantics - the job
-- has been handed off to the Soul: claimed -> dispatched is marked
-- ATOMICALLY BEFORE SendApply (a deliver-once intent marker). Once a row is
-- `dispatched`, the Reaper's recovery scan does NOT re-claim it (reclaim is
-- narrowed to `status='claimed'`, S4): after hand-off the run is owned by
-- the Soul, a repeat SendApply would be a double apply.
--
-- `running` is KEPT in the CHECK (vestigial): no longer used in the Acolyte
-- flow (claimed -> dispatched -> terminal instead of claimed -> running ->
-- terminal), but dropping a valid enum value is risky (old/ad-hoc rows
-- could still carry it). Extending the CHECK via drop+recreate is the
-- pattern from migrations 025/036 (status is a CHECK constraint, not a PG
-- enum, so it's reversible).
--
-- Status set after this migration:
--   planned / claimed / running / dispatched / success / failed / cancelled.
--
-- The partial index `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')) is left UNTOUCHED: dispatched rows are
-- not scanned by this index - reclaim after S4 only picks up claimed rows,
-- and correlateRunResult looks up by exact PK. Adding dispatched to the
-- index would be YAGNI.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'));
