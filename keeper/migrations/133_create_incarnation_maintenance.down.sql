-- 133_create_incarnation_maintenance.down.sql
--
-- Drops every maintenance window and freeze. After the rollback each incarnation
-- may start a run at any time, as before NIM-1072. Roll the binary back with it.

DROP TABLE IF EXISTS incarnation_freezes;
DROP TABLE IF EXISTS incarnation_maintenance_windows;
