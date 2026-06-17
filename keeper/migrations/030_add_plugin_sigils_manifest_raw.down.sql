-- 030_add_plugin_sigils_manifest_raw.down.sql
--
-- Откат M1-storage manifest_raw-колонки. Колонка аддитивная и nullable, поэтому
-- откат — простой DROP COLUMN: byte-exact канон verify теряется (после down
-- S6-verify сможет опираться только на JSONB-проекцию, что НЕ канон), но схема
-- возвращается к форме 029.

ALTER TABLE plugin_sigils
    DROP COLUMN IF EXISTS manifest_raw;
