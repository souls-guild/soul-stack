-- 105_rbac_roles_scope_mode.down.sql
--
-- Dropping the column takes both CHECK constraints with it. Nothing is lost that
-- authorization depends on: scope_mode records an INTENT and never took part in
-- resolution, so a catalog without it decides exactly the same way. What a pinned
-- role keeps is its materialized delta, which is where its ceiling actually lives.

ALTER TABLE rbac_roles
    DROP COLUMN IF EXISTS scope_mode;
