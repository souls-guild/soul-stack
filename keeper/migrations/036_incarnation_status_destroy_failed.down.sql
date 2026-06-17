-- 036_incarnation_status_destroy_failed.down.sql
--
-- Откат расширения enum `incarnation.status`. Перед DROP-ом старого CHECK-а надо
-- убедиться, что в таблице нет строк со статусом `destroy_failed` — иначе
-- ADD CONSTRAINT провалится. В down-миграции это не fail-safe (forward-only по
-- ADR-019): down предполагается только на свежей БД, где `destroy_failed`-строк
-- ещё нет. Возвращает CHECK к форме 031 (с `destroying`, без `destroy_failed`).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying'));
