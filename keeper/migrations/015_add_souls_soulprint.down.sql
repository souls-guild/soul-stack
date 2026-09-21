-- 015_add_souls_soulprint.down.sql

ALTER TABLE souls
    DROP COLUMN IF EXISTS soulprint_facts,
    DROP COLUMN IF EXISTS soulprint_collected_at,
    DROP COLUMN IF EXISTS soulprint_received_at;
