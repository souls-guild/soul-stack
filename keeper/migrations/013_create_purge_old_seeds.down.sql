-- 013_create_purge_old_seeds.down.sql

DROP FUNCTION IF EXISTS purge_old_seeds(text[], interval, integer);
