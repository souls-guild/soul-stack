-- 094_add_providers_fqdn_suffix.down.sql

ALTER TABLE providers
    DROP CONSTRAINT IF EXISTS providers_fqdn_suffix_format;

ALTER TABLE providers
    DROP COLUMN IF EXISTS fqdn_suffix;
