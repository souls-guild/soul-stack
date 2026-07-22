-- 089_add_incarnation_created_scenario.up.sql
--
-- Multiple create-scenario mechanism (Variant A): a service can declare
-- MULTIPLE start scenarios (`scenario/<name>/main.yml` with `create: true`),
-- and the operator picks one at POST /v1/incarnations (field
-- `create_scenario`, default `create`). This column stores the CHOICE as a
-- runtime fact of the incarnation: which scenario created it.
--
-- Why a column instead of deriving it from state_history: rerun-create (POST
-- .../rerun-create) must restart EXACTLY the scenario that created it. Before
-- this migration, rerun hardcoded `create` (incarnation.UnlockForRerun read
-- the last state_history.scenario and compared it to the string "create").
-- With multiple create scenarios this would break: an incarnation created by
-- `create_cluster` would restart `create` on rerun instead. created_scenario
-- is the stable, authoritative source of "what created it" (the last
-- state_history entry may carry a rerun-create/migration label, not the
-- bootstrap scenario's name).
--
-- NOT NULL DEFAULT 'create' - back-compat: all existing incarnations were
-- created via the default `create`, so backfilling with 'create' is correct,
-- and the DEFAULT covers rows inserted by old code during the transition
-- period. The scenario name is a snake_case verb (ScenarioNamePattern), TEXT
-- is sufficient; we do not add a separate format CHECK - name validation
-- lives on the keeper's request path, the DB does not duplicate the
-- application regex, same as for covens/traits.

ALTER TABLE incarnation
    ADD COLUMN created_scenario TEXT NOT NULL DEFAULT 'create';

COMMENT ON COLUMN incarnation.created_scenario IS
    'Name of the start scenario that created the incarnation (multiple create-scenario mechanism, Variant A). rerun-create restarts exactly this one. DEFAULT create - back-compat.';
