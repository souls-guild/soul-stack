-- 090_incarnation_created_scenario_nullable.down.sql
--
-- Reversible rollback: restores created_scenario to NOT NULL DEFAULT 'create'
-- (the state as of migration 089). Before restoring NOT NULL, a backfill of
-- NULL → 'create' must run first - otherwise ALTER ... SET NOT NULL would fail on
-- bare rows (NULL). The backfill is correct as a rollback: returning to the 089
-- union semantics makes the default `create` privileged again, so bare incarnations
-- collapse into 'create' (rerun-create becomes possible for them again - this is
-- exactly the previous behavior).

UPDATE incarnation
    SET created_scenario = 'create'
    WHERE created_scenario IS NULL;

ALTER TABLE incarnation
    ALTER COLUMN created_scenario SET DEFAULT 'create',
    ALTER COLUMN created_scenario SET NOT NULL;

COMMENT ON COLUMN incarnation.created_scenario IS
    'Name of the starting scenario that created the incarnation (mechanism for multiple create scenarios, Variant A). rerun-create restarts exactly this one. DEFAULT create -- back-compat.';
