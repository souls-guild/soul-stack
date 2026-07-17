-- 063_voyage_targets_dispatch_idx.up.sql
--
-- C1 (ADR-043): partial UNIQUE indexes on the dispatch references of voyage_targets for
-- back-link history -> Voyage. soul.SelectHistory LEFT JOINs voyage_targets on
-- apply_id (scenario) and errand_id (errand); without the index - a seq scan on every
-- per-host timeline page. The partial WHERE excludes NULL rows (target before
-- dispatch), the index covers only populated references. UNIQUE also
-- enforces at the DB level the invariant "one apply_id/errand_id -> at most one
-- voyage_targets row" (not just via the MarkTargetRunning write logic).
CREATE UNIQUE INDEX voyage_targets_apply_id_idx
    ON voyage_targets (apply_id) WHERE apply_id IS NOT NULL;

CREATE UNIQUE INDEX voyage_targets_errand_id_idx
    ON voyage_targets (errand_id) WHERE errand_id IS NOT NULL;
