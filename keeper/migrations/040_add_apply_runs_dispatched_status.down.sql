-- 040_add_apply_runs_dispatched_status.down.sql
--
-- Rolling back the `dispatched` phase from the `apply_runs.status` enum. Before narrowing the CHECK,
-- we move existing dispatched rows back to `running` - otherwise
-- ADD CONSTRAINT would fail on them. `running` is the closest in semantics
-- to a "dispatched/in progress" status in the pre-reform schema (vestigial, but valid),
-- so down doesn't lose rows or fail (symmetric with 036, but with a preceding
-- UPDATE - `incarnation.status` wasn't expected to have down-rows, here we're being cautious).
-- Restores the CHECK to the form from 025 (planned/claimed/running/success/failed/cancelled).

UPDATE apply_runs SET status = 'running' WHERE status = 'dispatched';

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'));
