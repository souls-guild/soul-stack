-- 082_add_incarnation_applying_epoch.down.sql
--
-- Revert of ADR-027 amend (m) S0: drops the applying-epoch columns + partial index.
-- Purely reversible - the columns are nullable, with no dependent constraints; at S0
-- nothing writes them yet. Drop the index first (it depends on the applying_since
-- column), then the columns.

DROP INDEX IF EXISTS incarnation_applying_scan_idx;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS applying_since,
    DROP COLUMN IF EXISTS applying_by_kid,
    DROP COLUMN IF EXISTS applying_attempt,
    DROP COLUMN IF EXISTS applying_apply_id;
