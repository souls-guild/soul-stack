-- 017_soulseeds_status_orphaned.down.sql
--
-- Откат расширения enum `soul_seeds.status`. См. комментарий в 016.down.sql:
-- forward-only (ADR-019), down предполагается только на свежей БД, где
-- `orphaned`-строк ещё нет.

ALTER TABLE soul_seeds
    DROP CONSTRAINT soul_seeds_status_valid;

ALTER TABLE soul_seeds
    ADD CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked'));
