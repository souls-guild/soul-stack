-- 136_maintenance_window_kinds.up.sql
--
-- NIM-1137 (epic NIM-1069). A maintenance window gains a kind: `weekly`, the
-- slot set of migration 133, or `once`, one interval [starts_at, ends_at). Both
-- live in the one list of an incarnation's windows. Each kind carries exactly
-- its own columns and NULL in the other kind's, so the weekly columns lose
-- NOT NULL and the CHECK below takes over; the existing per-column CHECKs pass
-- on NULL. Every row so far is weekly.
--
-- A once window counts only until it ends. The Reaper deletes one that ended
-- more than `maintenance.window_retention` ago, and a freeze that ended more
-- than `maintenance.freeze_retention` ago; the two indexes serve that scan.

ALTER TABLE incarnation_maintenance_windows
    ADD COLUMN kind      TEXT NOT NULL DEFAULT 'weekly',
    ADD COLUMN starts_at TIMESTAMPTZ,
    ADD COLUMN ends_at   TIMESTAMPTZ,
    ALTER COLUMN days             DROP NOT NULL,
    ALTER COLUMN start_time       DROP NOT NULL,
    ALTER COLUMN duration_minutes DROP NOT NULL,
    ALTER COLUMN timezone         DROP NOT NULL,
    ADD CONSTRAINT incarnation_maintenance_windows_kind_columns CHECK (
        (kind = 'weekly'
            AND days IS NOT NULL AND start_time IS NOT NULL
            AND duration_minutes IS NOT NULL AND timezone IS NOT NULL
            AND starts_at IS NULL AND ends_at IS NULL)
        OR (kind = 'once'
            AND days IS NULL AND start_time IS NULL
            AND duration_minutes IS NULL AND timezone IS NULL
            AND starts_at IS NOT NULL AND ends_at IS NOT NULL AND ends_at > starts_at));

CREATE INDEX incarnation_maintenance_windows_once_ends_idx
    ON incarnation_maintenance_windows (ends_at) WHERE kind = 'once';

CREATE INDEX incarnation_freezes_ends_idx ON incarnation_freezes (ends_at);

COMMENT ON TABLE incarnation_maintenance_windows IS
    'Maintenance windows of an incarnation (NIM-1072, NIM-1137): weekly slots or one-off intervals during which a run may start. No rows, or only one-off windows that have ended, means any time.';
