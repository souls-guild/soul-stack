-- 016_souls_status_destroyed.up.sql
--
-- ADR-017 cascade: добавляем терминальный статус `destroyed` к enum
-- `souls.status`. Используется keeper-side core-модулем
-- `core.cloud.provisioned destroyed` после успешного CloudDriver.Destroy
-- для VM, выпущенной той же связкой (cloud-create → cloud-destroy).
--
-- Семантика статусов после миграции:
--   * `pending` — оператор выписал bootstrap-токен, Soul ещё не пришёл.
--   * `connected` — стрим жив, Keeper держит lease в Redis.
--   * `disconnected` — стрим закрыт, lease истёк (Soul может вернуться).
--   * `revoked` — оператор отозвал, новые подключения отвергаются.
--   * `expired` — Жнец передвинул `pending` после TTL bootstrap-токена.
--   * `destroyed` — Soul-side хост физически удалён через
--     `core.cloud.provisioned destroyed`. Терминальный (no transitions out).
--     Forensic-state: НЕ включается в default-set `purge_souls.statuses`,
--     чтобы строка пережила инцидент-разбор. Оператор может удалить вручную.

ALTER TABLE souls
    DROP CONSTRAINT souls_status_valid;

ALTER TABLE souls
    ADD CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired', 'destroyed'));
