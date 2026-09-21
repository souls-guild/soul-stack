-- 085_operators_bootstrap_index.down.sql
--
-- Rollback of 085: reverts the bootstrap index to `created_by_aid IS NULL` (the form from
-- migration 003). Applied BEFORE the rollback of 084 (down chain runs in reverse order),
-- so the created_via column is still present - DROP/CREATE completes correctly.
--
-- WARNING: if the registry already has NON-bootstrap rows with created_by_aid IS NULL
-- (archon-system, federated operators), this rollback will reject them as a uniqueness
-- violation. The down path only applies when none exist.

DROP INDEX operators_first_archon_idx;
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_by_aid IS NULL;
