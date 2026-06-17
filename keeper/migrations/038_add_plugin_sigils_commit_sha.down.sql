-- 038_add_plugin_sigils_commit_sha.down.sql
--
-- Откат A1-S3 audit-колонки commit_sha. Колонка аддитивная и nullable, вне
-- подписи и вне verify/broadcast-пути, поэтому откат — простой DROP COLUMN:
-- теряется только audit-метка происхождения, схема возвращается к форме 037.

ALTER TABLE plugin_sigils
    DROP COLUMN IF EXISTS commit_sha;
