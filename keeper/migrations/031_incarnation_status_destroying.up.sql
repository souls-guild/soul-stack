-- 031_incarnation_status_destroying.up.sql
--
-- S-D1 (incarnation.destroy): добавляем статус `destroying` к enum
-- `incarnation.status`. Оператор инициирует destroy → строка переводится в
-- `destroying` → запускается teardown (scenario `destroy`, S-D2) → при успехе
-- DELETE строки (S-D3).
--
-- Семантика статусов после миграции:
--   * `ready`            — incarnation в рабочем состоянии, прогоны разрешены.
--   * `applying`         — идёт прогон scenario (lock на дальнейшие операции).
--   * `error_locked`     — сценарий упал частично, нужен unlock.
--   * `migration_failed` — state_schema-миграция упала, нужен unlock (ADR-019).
--   * `destroying`       — инициирован destroy: идёт teardown с последующим
--     DELETE строки. НЕ терминальный для строки (при успехе строка исчезает,
--     при фейле teardown → error_locked в S-D2). Из этого статуса run /
--     upgrade / повторный destroy отвергаются.

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying'));
