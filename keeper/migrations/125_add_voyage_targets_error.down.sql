-- 125_add_voyage_targets_error.down.sql
--
-- A target refused before its child run existed reads a bare `failed` again.

ALTER TABLE voyage_targets
    DROP COLUMN IF EXISTS error_message,
    DROP COLUMN IF EXISTS error_code;
