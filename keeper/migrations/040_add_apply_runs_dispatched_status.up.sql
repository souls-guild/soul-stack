-- 040_add_apply_runs_dispatched_status.up.sql
--
-- GATE-1 передизайн recovery (ADR-027 amend, S2): вводим фазу lifecycle
-- `dispatched` в enum `apply_runs.status`. Семантика — задание отдано Soul-у:
-- claimed → dispatched отмечается АТОМАРНО ПЕРЕД SendApply (deliver-once
-- intent-маркер). Как только строка `dispatched`, recovery-скан Reaper-а её НЕ
-- пере-claim-ит (reclaim сужен до `status='claimed'`, S4): после отдачи прогон
-- ведёт Soul, повторный SendApply = двойной apply.
--
-- `running` СОХРАНЯЕМ в CHECK (vestigial): в Acolyte-флоу больше не используется
-- (claimed → dispatched → terminal вместо claimed → running → terminal), но
-- удалять допустимое значение enum рискованно (старые/ad-hoc строки могли его
-- нести). Расширение CHECK через drop+recreate — паттерн миграций 025/036
-- (status — CHECK-constraint, не PG-enum, значит обратим).
--
-- Множество статусов после миграции:
--   planned / claimed / running / dispatched / success / failed / cancelled.
--
-- partial-индекс `apply_runs_claim_scan_idx` (025, WHERE status IN
-- ('planned','claimed','running')) НЕ трогаем: dispatched-строки этим индексом
-- не сканируются — reclaim после S4 берёт только claimed, correlateRunResult
-- ищет по точному PK. Добавлять dispatched в индекс — YAGNI.

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'dispatched', 'success', 'failed', 'cancelled'));
