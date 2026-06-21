-- 082_add_incarnation_applying_epoch.down.sql
--
-- Откат ADR-027 amend (m) S0: снимаем applying-epoch колонки + partial-индекс.
-- Чисто обратимо — колонки nullable, без зависимых constraint-ов; на S0
-- никем не пишутся. Сначала индекс (зависит от колонки applying_since),
-- затем колонки.

DROP INDEX IF EXISTS incarnation_applying_scan_idx;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS applying_since,
    DROP COLUMN IF EXISTS applying_by_kid,
    DROP COLUMN IF EXISTS applying_attempt,
    DROP COLUMN IF EXISTS applying_apply_id;
