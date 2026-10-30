-- 136_maintenance_window_kinds.down.sql
--
-- A once window has no weekly form, so going down deletes them.

DROP INDEX IF EXISTS incarnation_freezes_ends_idx;

DELETE FROM incarnation_maintenance_windows WHERE kind <> 'weekly';

ALTER TABLE incarnation_maintenance_windows
    DROP CONSTRAINT IF EXISTS incarnation_maintenance_windows_kind_columns,
    DROP COLUMN IF EXISTS ends_at,
    DROP COLUMN IF EXISTS starts_at,
    DROP COLUMN IF EXISTS kind,
    ALTER COLUMN days             SET NOT NULL,
    ALTER COLUMN start_time       SET NOT NULL,
    ALTER COLUMN duration_minutes SET NOT NULL,
    ALTER COLUMN timezone         SET NOT NULL;

COMMENT ON TABLE incarnation_maintenance_windows IS
    'Maintenance windows of an incarnation (NIM-1072): weekly slots during which a run may start. No rows means any time.';
