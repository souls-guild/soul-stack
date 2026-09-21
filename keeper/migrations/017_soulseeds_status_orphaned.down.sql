-- 017_soulseeds_status_orphaned.down.sql
--
-- Rollback of the `soul_seeds.status` enum extension. See the comment in 016.down.sql:
-- forward-only (ADR-019), down is only expected to run on a fresh DB where
-- there are no `orphaned` rows yet.

ALTER TABLE soul_seeds
    DROP CONSTRAINT soul_seeds_status_valid;

ALTER TABLE soul_seeds
    ADD CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked'));
