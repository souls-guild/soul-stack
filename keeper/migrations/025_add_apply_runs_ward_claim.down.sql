-- 025_add_apply_runs_ward_claim.down.sql
--
-- Откат ADR-027 Phase 0. status — CHECK-constraint (НЕ PG-enum), поэтому
-- расширение значениями planned/claimed ПОЛНОСТЬЮ обратимо: возвращаем
-- constraint к форме 018+024 (running/success/failed/cancelled). Оговорка про
-- необратимость `ALTER TYPE ... ADD VALUE` здесь НЕ применяется — enum-а нет.
--
-- Предусловие down: в apply_runs не должно остаться строк со status в
-- ('planned','claimed') — иначе recreate CHECK упадёт (23514). В Phase 0 эти
-- значения никем не пишутся, так что на чистом Phase 0 откат безопасен.

DROP INDEX IF EXISTS apply_runs_claim_scan_idx;

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('running', 'success', 'failed', 'cancelled'));

ALTER TABLE apply_runs
    DROP COLUMN IF EXISTS attempt,
    DROP COLUMN IF EXISTS claim_expires_at,
    DROP COLUMN IF EXISTS claim_at,
    DROP COLUMN IF EXISTS claim_by_kid;
