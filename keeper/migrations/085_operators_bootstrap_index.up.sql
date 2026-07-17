-- 085_operators_bootstrap_index.up.sql
--
-- ADR-058(d): move the bootstrap invariant (exactly one first Archon) from
-- `created_by_aid IS NULL` (migration 003) to `created_via = 'bootstrap'`
-- (column from migration 084).
--
-- The previous index enforced "a single row with created_by_aid IS NULL"; this
-- blocked seeding `archon-system` (created_by_aid = NULL) and federated operators
-- (auto-provision without an initiating operator -> created_by_aid = NULL). After
-- the move, NULL in created_by_aid is legal for NON-bootstrap rows; uniqueness
-- is guaranteed only for `created_via = 'bootstrap'`.
--
-- We do NOT touch the CHECK `self_reference_ok` (migration 003) - it stays.

DROP INDEX operators_first_archon_idx;
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_via = 'bootstrap';
