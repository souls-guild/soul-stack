-- 103_add_engine_provenance.down.sql

ALTER TABLE state_history
    DROP COLUMN IF EXISTS engine_compat;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS engine_compat;

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS soul_version,
    DROP COLUMN IF EXISTS keeper_version;
