-- 077_create_purge_archives.down.sql

DROP FUNCTION IF EXISTS purge_incarnation_archive(interval, integer);
DROP FUNCTION IF EXISTS purge_state_history_archive(interval, integer);
DROP FUNCTION IF EXISTS purge_archived_state_history(interval, integer);
