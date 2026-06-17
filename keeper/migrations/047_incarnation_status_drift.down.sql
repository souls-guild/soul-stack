-- 047_incarnation_status_drift.down.sql
--
-- Откат ADR-031 (Scry): удаляем `drift` из enum `incarnation.status`. Down
-- ассумит отсутствие строк со status='drift' (миграция down вызывается
-- only после ручной проверки/восстановления).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed'));
