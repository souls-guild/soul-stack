-- 093_create_purge_old_certs.down.sql

DROP FUNCTION IF EXISTS purge_old_certs(text[], interval, integer);
