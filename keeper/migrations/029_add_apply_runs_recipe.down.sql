-- 029_add_apply_runs_recipe.down.sql
--
-- Rollback of ADR-027(c)(f) Phase 1 recipe column. The column is additive
-- and nullable, so the rollback is a simple DROP COLUMN: recipe data is
-- lost (job cannot be restored after down), but the schema reverts to the
-- form of 028.

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS recipe;
