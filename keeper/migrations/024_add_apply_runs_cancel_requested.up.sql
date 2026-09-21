-- 024_add_apply_runs_cancel_requested.up.sql
--
-- Cluster-wide Cancel for a run (HA fix G1). Problem: the run's run-goroutine
-- lives in the memory of a SINGLE Keeper instance (ADR-002, stateless cluster),
-- while Runner.Cancel only cancels the local runCtx. If the operator calls Cancel
-- on Keeper-B while the run-goroutine is on Keeper-A - the cancellation never arrives.
--
-- Mechanism - a PG flag (multi-instance-safe) that piggybacks on the existing
-- barrier polling: the run-goroutine already polls apply_runs in waitBarrier
-- (SelectStatusesByApplyID, dispatch.go), and now also reads
-- cancel_requested. Any Keeper sets the flag on Cancel
-- (UPDATE ... SET cancel_requested=true); the goroutine-owning instance sees it
-- on the next barrier tick and cancels itself - through the same abort path as
-- a local Cancel. No new Redis channel needed; survives cross-Keeper; consistent
-- with the PG fan-in model of a run.
--
-- The flag is only set on running rows (terminal rows are untouched -> cancelling
-- an already-finished run is a no-op). The polling period (defaultPollInterval =
-- 200ms) is the upper bound on cancellation latency - acceptable.
--
-- NOT NULL DEFAULT false: existing rows get false, new dispatches
-- start without a requested cancellation. Forward-only (ADR-007 migrations).

ALTER TABLE apply_runs
    ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN apply_runs.cancel_requested IS
    'Cluster-wide Cancel (G1): any Keeper sets true; the run-goroutine owner sees the flag in barrier polling and cancels the run. Running rows only.';
