-- 126_state_history_error_detail.down.sql
--
-- Drops the structured reason; `error_summary` keeps the text. Roll the binary
-- back with it: a keeper that knows migration 126 writes and reads the column,
-- and against a table without it every run terminal and every runs read fails.

ALTER TABLE state_history DROP COLUMN IF EXISTS error_detail;
