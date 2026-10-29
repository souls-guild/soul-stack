-- 133_create_incarnation_maintenance.up.sql
--
-- NIM-1072 (epic NIM-1069). An incarnation's maintenance windows and freezes:
-- each incarnation has its own, there is no shared named window. A window is a
-- weekly slot set - on each listed day a slot starts at `start_time` wall clock
-- in `timezone` and lasts `duration_minutes` of elapsed time - and gates when a
-- run may START. A freeze is a one-off [starts_at, ends_at) during which no
-- automatic run starts. keeper/internal/maintenance validates both and computes
-- "open now / next start" from them.
--
-- What the schema cannot hold, and the code checks on write instead: that
-- `timezone` names a zone the Keeper can load (`Local` is refused), and that
-- an incarnation has at most 50 windows and 200 freezes.
--
-- Both cascade from the incarnation: a window outlives nothing it belongs to.

CREATE TABLE incarnation_maintenance_windows (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    incarnation_id   TEXT        NOT NULL REFERENCES incarnation (id) ON DELETE CASCADE,
    days             TEXT[]      NOT NULL,
    start_time       TEXT        NOT NULL,
    duration_minutes INT         NOT NULL,
    timezone         TEXT        NOT NULL,
    comment          TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incarnation_maintenance_windows_days_valid
        CHECK (cardinality(days) BETWEEN 1 AND 7
               AND days <@ ARRAY['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun']::TEXT[]),
    CONSTRAINT incarnation_maintenance_windows_start_time_format
        CHECK (start_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    CONSTRAINT incarnation_maintenance_windows_duration_range
        CHECK (duration_minutes BETWEEN 1 AND 1440),
    CONSTRAINT incarnation_maintenance_windows_timezone_len
        CHECK (char_length(timezone) BETWEEN 1 AND 64),
    CONSTRAINT incarnation_maintenance_windows_comment_len
        CHECK (char_length(comment) <= 500)
);

CREATE INDEX incarnation_maintenance_windows_incarnation_idx
    ON incarnation_maintenance_windows (incarnation_id, created_at);

COMMENT ON TABLE incarnation_maintenance_windows IS
    'Maintenance windows of an incarnation (NIM-1072): weekly slots during which a run may start. No rows means any time.';

CREATE TABLE incarnation_freezes (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    incarnation_id TEXT        NOT NULL REFERENCES incarnation (id) ON DELETE CASCADE,
    starts_at      TIMESTAMPTZ NOT NULL,
    ends_at        TIMESTAMPTZ NOT NULL,
    comment        TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT incarnation_freezes_interval_valid CHECK (ends_at > starts_at),
    CONSTRAINT incarnation_freezes_comment_len CHECK (char_length(comment) <= 500)
);

CREATE INDEX incarnation_freezes_incarnation_idx ON incarnation_freezes (incarnation_id, starts_at);

COMMENT ON TABLE incarnation_freezes IS
    'Freezes of an incarnation (NIM-1072): [starts_at, ends_at) during which no automatic run starts, whatever the windows say.';
