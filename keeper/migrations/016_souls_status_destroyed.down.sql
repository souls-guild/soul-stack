-- 016_souls_status_destroyed.down.sql
--
-- Откат расширения enum `souls.status`. Перед DROP-ом старого CHECK-а
-- надо убедиться, что в таблице нет строк со статусом `destroyed` —
-- иначе ADD CONSTRAINT провалится. В down-миграции это не fail-safe
-- (forward-only по ADR-019), поэтому каста к `revoked` нет: down
-- предполагается только на свежей БД, где `destroyed`-строк ещё нет.

ALTER TABLE souls
    DROP CONSTRAINT souls_status_valid;

ALTER TABLE souls
    ADD CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired'));
