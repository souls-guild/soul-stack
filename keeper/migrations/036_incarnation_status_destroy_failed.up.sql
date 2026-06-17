-- 036_incarnation_status_destroy_failed.up.sql
--
-- S-D2a (incarnation.destroy): добавляем терминальный статус `destroy_failed`
-- к enum `incarnation.status`. Семантика — teardown (scenario `destroy`, S-D2b)
-- упал на хостах: инстанс НЕ удалён, state остался last known-good, требуется
-- вмешательство оператора (повторить destroy / force-снести / unlock в ready).
--
-- Сам переход в `destroy_failed` выставляется в S-D2b/S-D3 (teardown-исход);
-- эта миграция вводит только допустимое значение enum. Расширение CHECK через
-- drop+recreate — паттерн 016/017/031 (status — CHECK-constraint, не PG-enum,
-- значит обратим).
--
-- Семантика статусов после миграции:
--   * `ready`            — incarnation в рабочем состоянии, прогоны разрешены.
--   * `applying`         — идёт прогон scenario (lock на дальнейшие операции).
--   * `error_locked`     — сценарий упал частично, нужен unlock.
--   * `migration_failed` — state_schema-миграция упала, нужен unlock (ADR-019).
--   * `destroying`       — инициирован destroy: идёт teardown с последующим
--     DELETE строки (S-D1/S-D2b/S-D3).
--   * `destroy_failed`   — teardown упал: инстанс НЕ удалён, нужно вмешательство
--     оператора. Терминальный для строки до явного действия оператора.

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed'));
