-- 043_mark_disconnected_lease_aware.down.sql

DROP FUNCTION IF EXISTS mark_connected_sids(text[]);
DROP FUNCTION IF EXISTS select_reconnect_candidates(integer);
DROP FUNCTION IF EXISTS mark_disconnected_sids(text[]);
DROP FUNCTION IF EXISTS select_disconnect_candidates(interval, integer);
