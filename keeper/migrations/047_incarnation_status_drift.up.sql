-- 047_incarnation_status_drift.up.sql
--
-- ADR-031 (Scry): добавляем информационный статус `drift` в enum
-- `incarnation.status`. Семантика — Scry-проверка обнаружила расхождение
-- декларированного состояния с фактом хотя бы на одном хосте; статус НЕ
-- блокирует remediation (в отличие от `error_locked`/`destroy_failed`),
-- remediation drift-а = обычный apply из `drift` → `ready`.
--
-- Сам переход в `drift` выставляется check-drift-handler-ом после успешной
-- сборки `DriftReport` (если есть расхождения); эта миграция вводит только
-- допустимое значение enum. Расширение CHECK через drop+recreate — паттерн
-- 016/017/031/036 (status — CHECK-constraint, не PG-enum, значит обратим).
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
--   * `drift`            — Scry-check обнаружил расхождение реального состояния
--     с декларацией (ADR-031, информационный, НЕ блокирующий).

ALTER TABLE incarnation
    DROP CONSTRAINT incarnation_status_valid;

ALTER TABLE incarnation
    ADD CONSTRAINT incarnation_status_valid
        CHECK (status IN ('ready', 'applying', 'error_locked', 'migration_failed', 'destroying', 'destroy_failed', 'drift'));
