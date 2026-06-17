-- 029_add_apply_runs_recipe.down.sql
--
-- Откат ADR-027(c)(f) Phase 1 recipe-колонки. Колонка аддитивная и nullable,
-- поэтому откат — простой DROP COLUMN: данные рецептов теряются (восстановление
-- задания после down невозможно), но схема возвращается к форме 028.

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS recipe;
