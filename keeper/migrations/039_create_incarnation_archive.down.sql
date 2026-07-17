-- 039_create_incarnation_archive.down.sql
--
-- Rollback of the S-D3 archive tables. The compliance-data archive is lost entirely -
-- down is intended for dev/CI-reset, not for prod (tearing down the archive of removed
-- incarnations is irreversible). These tables have no dependencies (no incoming FKs),
-- so a plain DROP in any order is fine.

DROP TABLE IF EXISTS state_history_archive;
DROP TABLE IF EXISTS incarnation_archive;
