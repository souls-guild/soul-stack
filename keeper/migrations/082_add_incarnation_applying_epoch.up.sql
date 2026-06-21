-- 082_add_incarnation_applying_epoch.up.sql
--
-- ADR-027 amend (m), слайс S0 (фундамент): epoch для applying-флага инкарнации
-- под Reaper-правило reconcile_orphan_applying (standalone-orphan reconcile).
--
-- ПРОБЛЕМА. Прямой incarnation.run (standalone, не под Voyage) ставит
-- incarnation.status='applying' в lockRun; если Keeper-владелец прогона умирает
-- до терминала — lock виснет НАВСЕГДА. Voyage-путь закрыт amend (l)
-- (orphan-release по back-link voyage_targets.apply_id), но прямой run строки
-- voyage_targets НЕ имеет — back-link структурно недостижим. reclaim_apply_runs
-- сюда тоже не достаёт (реклеймит protухший claimed-Ward в apply_runs, а
-- applying-lock — отдельный флаг на строке incarnation). Корень: applying-флаг —
-- бесхозный bool без epoch / владельца / lease.
--
-- РЕШЕНИЕ S0 — ТОЛЬКО схема: четыре NULLABLE-колонки дают applying-флагу epoch.
-- Запись epoch в lockRun + очистка на терминале + само Reaper-правило — слайс
-- S1. После S0 эти колонки НИКЕМ не пишутся и не читаются.
--
--   - applying_apply_id — apply_id прогона, держащего lock; NULL пока не applying.
--   - applying_attempt  — fencing-epoch прогона (parity apply_runs.attempt).
--   - applying_by_kid   — KID Keeper-владельца прогона; presence-чек в Conclave
--                         (InstanceAlive) различает «прогон идёт» vs «владелец
--                         мёртв, lock осиротел».
--   - applying_since    — момент взятия lock; правило ищет stale-кандидатов по
--                         age (applying_since < NOW() - stale_after, 90s parity
--                         mark_disconnected).
--
-- АДДИТИВНОСТЬ / forward-only (ADR-007). Все колонки nullable, без backfill: у
-- существующих applying-строк epoch неизвестен (NULL applying_by_kid) — правило
-- такие НЕ реклеймит (fail-safe, чтобы не сорвать прогон с неизвестным epoch).

ALTER TABLE incarnation
    ADD COLUMN applying_apply_id TEXT,
    ADD COLUMN applying_attempt  INTEGER,
    ADD COLUMN applying_by_kid   TEXT,
    ADD COLUMN applying_since    TIMESTAMPTZ;

-- Partial-индекс под Reaper-scan stale-кандидатов (parity apply_runs_claim_scan_idx,
-- миграция 025): правило сканирует ровно applying-строки по applying_since-age.
-- WHERE-предикат держит индекс узким (только applying — единицы/десятки строк
-- на кластер), терминальные/ready исключены. Заложен в S0 — на текущем пути
-- applying_since NULL, индекс корректен и безвреден.
CREATE INDEX incarnation_applying_scan_idx
    ON incarnation (status, applying_since)
    WHERE status = 'applying';

COMMENT ON COLUMN incarnation.applying_apply_id IS
    'standalone-orphan epoch (ADR-027(m)): apply_id прогона, держащего applying-lock; NULL пока не applying. S0 — не пишется.';
COMMENT ON COLUMN incarnation.applying_attempt IS
    'standalone-orphan epoch (ADR-027(m)): fencing-epoch прогона (parity apply_runs.attempt). S0 — не пишется.';
COMMENT ON COLUMN incarnation.applying_by_kid IS
    'standalone-orphan epoch (ADR-027(m)): KID Keeper-владельца прогона; presence-чек в Conclave (InstanceAlive) отличает живой прогон от осиротевшего lock-а. S0 — не пишется.';
COMMENT ON COLUMN incarnation.applying_since IS
    'standalone-orphan epoch (ADR-027(m)): момент взятия applying-lock; reconcile_orphan_applying ищет stale-кандидатов по age (stale_after=90s). S0 — не пишется.';
