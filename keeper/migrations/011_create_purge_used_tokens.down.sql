-- 011_create_purge_used_tokens.down.sql

DROP FUNCTION IF EXISTS purge_used_tokens(interval, integer);
