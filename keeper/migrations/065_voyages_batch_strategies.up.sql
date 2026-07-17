-- 065_voyages_batch_strategies.up.sql
--
-- ADR-043 amendment (2026-06-01) -> S-W3/S-W4: Salt-level batch strategies for
-- Voyage. Four additive nullable columns in `voyages` (forward-compat: runs
-- without the fields work as before, ADR-012 forward-compat only-add). Resolving NULL->
-- default happens handler/orchestrator-side, not via a column default, so that "not
-- set" stays distinguishable from an explicit value in audit/UI (parity with the batch_mode
-- of migration 064).
--
--   * batch_percent - batch size as a % of the resolved scope (parity with Salt
--     `-b 25%`), MUTUALLY EXCLUSIVE with batch_size. Stored for audit/UI; the handler,
--     when present, computes the effective batch_size = ceil(scope * pct/100) and
--     writes it to batch_size (the resolver/orchestrator sees a plain batch_size).
--     Only meaningful for batch_mode=barrier (unused in window). 1..100.
--   * fail_threshold - a threshold on the absolute failure count: once N failures accumulate ->
--     the run stops (no new Legs / window units start). on_failure
--     =abort is equivalent to fail_threshold:1; on_failure=continue is equivalent to NULL (no threshold). N>1 is
--     intermediate tolerance. Works in both batch_mode values. > 0.
--   * inter_unit_interval - a per-unit pause in batch_mode=window before spawning
--     the next window unit (parity with inter_batch_interval between Legs in
--     barrier). Type INTERVAL (like inter_batch_interval). Applies only to window.
--   * require_alive - a presence filter: when true, scope resolution excludes Souls without
--     a live presence lease (SoulLeaseChecker, ADR-006). The post-filter snapshot
--     is recorded in target_resolved (the snapshot invariant isn't weakened). NULL => false.
--
-- CHECK invariants:
--   * batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100).
--   * fail_threshold IS NULL OR fail_threshold > 0.
--   * The batch_size / batch_percent mutual exclusion is a handler invariant (exactly one
--     -> 422), NOT a CHECK: "both NULL" (the whole run as a single Leg) is also valid, which
--     an XOR CHECK can't express without false rejections.

ALTER TABLE voyages
    ADD COLUMN batch_percent       INT,
    ADD COLUMN fail_threshold      INT,
    ADD COLUMN inter_unit_interval INTERVAL,
    ADD COLUMN require_alive       BOOLEAN;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_batch_percent_range
        CHECK (batch_percent IS NULL OR (batch_percent >= 1 AND batch_percent <= 100));

ALTER TABLE voyages
    ADD CONSTRAINT voyages_fail_threshold_positive
        CHECK (fail_threshold IS NULL OR fail_threshold > 0);

COMMENT ON COLUMN voyages.batch_percent IS
    'Batch size as % of scope (ADR-043 amendment 2026-06-01), XOR with batch_size. 1..100; meaningful for batch_mode=barrier. NULL => batch_size is set (or the whole run is a single Leg).';
COMMENT ON COLUMN voyages.fail_threshold IS
    'Absolute failure-count threshold -> stop (ADR-043 amendment 2026-06-01). abort == 1; continue == NULL. Works in both batch_mode values.';
COMMENT ON COLUMN voyages.inter_unit_interval IS
    'Per-unit pause in batch_mode=window before spawning the next unit (ADR-043 amendment 2026-06-01, parity inter_batch_interval). NULL => no pause.';
COMMENT ON COLUMN voyages.require_alive IS
    'Presence filter for live Souls when resolving scope (ADR-043 amendment 2026-06-01, SoulLeaseChecker). NULL => false (no filter).';
