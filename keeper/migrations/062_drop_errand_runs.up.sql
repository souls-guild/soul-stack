-- 062_drop_errand_runs.up.sql
--
-- Wave 5 Pass 2: полное удаление ErrandRun (ADR-041). Multi-target обвязка над
-- Errand вплавлена в Voyage (kind=command покрывает массовый pull-ad-hoc exec),
-- отдельная сущность снята без обратной совместимости — реестр `errand_runs` и
-- back-link errands.errand_run_id больше не используются ни одним потребителем.
--
-- СНАЧАЛА снимаем back-link c errands (FK + колонка + индекс), потом DROP TABLE
-- errand_runs (образец — down.sql миграции 057). Таблица `errands` (single
-- Errand, ADR-033) ОСТАЁТСЯ.

DROP INDEX IF EXISTS errands_errand_run_id_idx;
ALTER TABLE errands DROP CONSTRAINT IF EXISTS errands_errand_run_id_fkey;
ALTER TABLE errands DROP COLUMN IF EXISTS errand_run_id;

DROP INDEX IF EXISTS errand_runs_claim_scan_idx;
DROP INDEX IF EXISTS errand_runs_pending_pickup_idx;
DROP TABLE IF EXISTS errand_runs;
