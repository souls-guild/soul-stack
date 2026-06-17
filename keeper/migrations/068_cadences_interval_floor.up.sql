-- 068_cadences_interval_floor.up.sql
--
-- ADR-046 Pass B (floor-лимит, амендмент 2026-06-07): минимальный период
-- interval-Cadence — 30s. Для реактивного запуска быстрее 30s — Beacons
-- (Vigil/Oracle, ADR-030), не Cadence.
--
-- Три части:
--   1. Partial-индекс cadences_enabled_interval_idx на (interval_seconds)
--      WHERE enabled — под адаптивный MIN-запрос Conductor (ADR-048 «Adaptive
--      interval»): `SELECT MIN(interval_seconds), bool_or(...) FROM cadences
--      WHERE enabled`. До него агрегат шёл seq-scan-ом по всей таблице; partial-
--      индекс по enabled-строкам отдаёт MIN без полного скана. Отличается от
--      cadences_due_scan_idx (066) — тот по (next_run_at) под due-выборку, этот по
--      (interval_seconds) под MIN-агрегат.
--   2. CHECK cadences_interval_seconds_floor: interval_seconds >= 30 (отдельным
--      именем, НЕ переопределяет cadences_interval_seconds_positive из 066 —
--      positive остаётся базовым sane-bound, floor ужесточает нижнюю границу).
--      cron-Cadence (interval_seconds NULL) под CHECK не попадают (NULL OR …).
--   3. Pre-flight data-guard ПЕРЕД ADD CONSTRAINT: если в таблице уже есть строки
--      с interval_seconds < 30 (напр. dev-стенд с 10s-cadence) — RAISE EXCEPTION с
--      понятным текстом. НЕ тихий UPDATE: молчаливое изменение периода чужого
--      расписания недопустимо (оператор сам решает — поднять до 30s, перевести в
--      cron или удалить). fail-fast: миграция останавливается, состояние не
--      меняется (вся up-миграция применяется в одной транзакции мигратора).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM cadences WHERE interval_seconds < 30) THEN
        RAISE EXCEPTION 'найдены cadences с interval_seconds < 30 — исправьте/удалите перед миграцией (минимум 30s; для суб-30s — Beacons)';
    END IF;
END $$;

ALTER TABLE cadences
    ADD CONSTRAINT cadences_interval_seconds_floor
        CHECK (interval_seconds IS NULL OR interval_seconds >= 30);

CREATE INDEX cadences_enabled_interval_idx
    ON cadences (interval_seconds)
    WHERE enabled;

COMMENT ON CONSTRAINT cadences_interval_seconds_floor ON cadences IS
    'Floor-лимит периода interval-Cadence (ADR-046 Pass B): interval_seconds >= 30s. Для реакции быстрее 30s — Beacons (ADR-030). cron-Cadence (NULL) не затрагиваются.';
