-- 090_incarnation_created_scenario_nullable.up.sql
--
-- Bare incarnation via NULL (create-variant Phase 2). Migration 089 introduced
-- created_scenario as TEXT NOT NULL DEFAULT 'create' (a back-compat union, when
-- the default `create` was privileged and always valid). Phase 2 removed the
-- hardcoded union: the set of starting scenarios is EXACTLY {scenarios with `create: true`},
-- the name `create` is no longer privileged. A new incarnation class appeared --
-- BARE: a service with no create scenario at all is created as StatusReady WITHOUT a run
-- (ready for operational use). For bare, the column must carry NULL (no bootstrap scenario),
-- not a made-up 'create'.
--
-- DROP NOT NULL + DROP DEFAULT: NULL = bare (no creating scenario), non-empty =
-- the bootstrap scenario's name. rerun-create on NULL is rejected (incarnation.UnlockForRerun
-- -> ErrRerunScenarioNotCreate -> 409): there's nothing to rerun.
--
-- We don't touch existing incarnations: rows with the value 'create' (inserted before
-- this migration by the 089 default) remain correct -- the redis service's scenario/
-- create/main.yml now carries `create: true` (Phase 1), so 'create' is still a
-- valid bootstrap for them, not a legacy artifact. No backfill needed.

ALTER TABLE incarnation
    ALTER COLUMN created_scenario DROP NOT NULL,
    ALTER COLUMN created_scenario DROP DEFAULT;

COMMENT ON COLUMN incarnation.created_scenario IS
    'Name of the starting scenario that created the incarnation (the multiple create-scenarios mechanism, Variant A). NULL = bare incarnation (created without a bootstrap scenario, StatusReady with no run). rerun-create restarts exactly this one (rejects with 409 on NULL).';
