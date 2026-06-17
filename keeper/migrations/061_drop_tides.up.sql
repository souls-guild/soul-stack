-- 061_drop_tides.up.sql
--
-- Wave 5 Pass 1: полное удаление Tide (ADR-040). Tide вплавлен в incarnation.run
-- (Voyage / ErrandRun покрывают батчевые сценарии), отдельная сущность снята без
-- обратной совместимости — реестр `tides` и back-link-колонки apply_runs больше
-- не используются ни одним потребителем.
--
-- apply_runs.tide_id / surge_index — soft-link БЕЗ FK (миграция 055): drop
-- колонок безопасен, на них нет внешних ссылок. partial-индекс apply_runs_tide_idx
-- снимаем вместе с колонкой.

DROP INDEX IF EXISTS apply_runs_tide_idx;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS surge_index;
ALTER TABLE apply_runs DROP COLUMN IF EXISTS tide_id;
DROP TABLE IF EXISTS tides;
