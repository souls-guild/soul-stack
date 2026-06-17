-- 066_create_cadences.down.sql
--
-- Down: снять back-link voyages.cadence_id (индекс/FK/колонка) и дропнуть
-- таблицу cadences с её индексом.

DROP INDEX IF EXISTS voyages_cadence_id_idx;

ALTER TABLE voyages
    DROP CONSTRAINT IF EXISTS voyages_cadence_id_fk;

ALTER TABLE voyages
    DROP COLUMN IF EXISTS cadence_id;

DROP INDEX IF EXISTS cadences_due_scan_idx;
DROP TABLE IF EXISTS cadences;
