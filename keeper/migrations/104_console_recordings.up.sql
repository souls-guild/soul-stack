-- 104_console_recordings.up.sql
--
-- Mandatory session recording of the console plane (ADR-0074(g), NIM-145).
--
-- A console is the only operator action whose effect cannot be reconstructed
-- from its request: `console.opened` says a shell was opened, and nothing says
-- what was typed. Recording is therefore not a feature flag — a session that
-- cannot be recorded is not opened, and an operator cannot choose an unrecorded
-- shell. That makes this table part of the console's authorization story rather
-- than an observability extra.
--
-- Postgres, not a file on the Keeper's disk: Keeper is a stateless
-- horizontally-scalable cluster (ADR-002/ADR-005), so a recording on the local
-- disk of the instance that happened to hold the socket is unreadable by the
-- instance that later serves the playback request (NIM-148).
--
-- The body is asciicast v2 (https://docs.asciinema.org/manual/asciicast/v2/):
-- a JSON header plus one JSON array per line. It is split across
-- `console_recording_parts` rather than accumulated in one column because the
-- writer appends while the session is live — a single growing TEXT would rewrite
-- the whole value on every flush, and an aborted session would leave nothing at
-- all.

CREATE TABLE console_recordings (
    recording_id TEXT PRIMARY KEY,
    -- session_id — the Keeper-minted console session ULID. A one-shot
    -- `keeper.soul.run-command` has no session and mints one of its own; the
    -- errand that carried it is reached through the `console.command` audit
    -- event, whose payload holds both ids. Unique: one session has exactly one
    -- recording, which is what makes "was this session recorded" answerable by
    -- a lookup rather than by a scan.
    session_id   TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    sid          TEXT        NOT NULL,
    archon_aid   TEXT        NOT NULL,
    -- cast_header — the asciicast v2 header object, kept structured so a reader
    -- can size a terminal without parsing the body.
    cast_header  JSONB       NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at  TIMESTAMPTZ,
    close_reason TEXT,
    event_count  BIGINT      NOT NULL DEFAULT 0,
    byte_count   BIGINT      NOT NULL DEFAULT 0,
    -- truncated — the session hit the per-session recording cap. It is closed
    -- when that happens (an unrecorded console may not keep running), so this
    -- marks a recording that ends before its shell did.
    truncated    BOOLEAN     NOT NULL DEFAULT FALSE,
    -- ttl_at — `started_at + console.recording.retention`, baked in on INSERT
    -- the way `errands.ttl_at` is (migration 052), so the Reaper's purge is one
    -- indexed DELETE and retention changes never rewrite existing rows.
    ttl_at       TIMESTAMPTZ NOT NULL,
    CONSTRAINT console_recordings_kind_valid CHECK (kind IN ('interactive', 'command')),
    CONSTRAINT console_recordings_archon_aid_fk
        FOREIGN KEY (archon_aid) REFERENCES operators (aid) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX console_recordings_session_idx
    ON console_recordings (session_id);

CREATE INDEX console_recordings_sid_started_idx
    ON console_recordings (sid, started_at DESC);

CREATE INDEX console_recordings_aid_started_idx
    ON console_recordings (archon_aid, started_at DESC);

CREATE INDEX console_recordings_ttl_idx
    ON console_recordings (ttl_at);

CREATE TABLE console_recording_parts (
    recording_id TEXT   NOT NULL,
    -- seq — append order, dense from 0. Playback is
    -- `ORDER BY seq` and a plain concatenation of `body`.
    seq          BIGINT NOT NULL,
    -- body — one or more complete asciicast v2 event lines, newline-terminated.
    -- A part never splits a line, so a reader may stream parts straight out
    -- without re-joining.
    body         TEXT   NOT NULL,
    PRIMARY KEY (recording_id, seq),
    CONSTRAINT console_recording_parts_recording_fk
        FOREIGN KEY (recording_id) REFERENCES console_recordings (recording_id) ON DELETE CASCADE
);

COMMENT ON TABLE console_recordings IS
    'One row = one recorded console session (ADR-0074(g), NIM-145). Recording is mandatory: a session that cannot be recorded is refused.';

COMMENT ON COLUMN console_recordings.kind IS
    'interactive = a PTY session over GET /v1/console; command = a one-shot keeper.soul.run-command (NIM-147), which shares the right and the record but not the transport.';

COMMENT ON COLUMN console_recordings.finished_at IS
    'NULL while the session is live, and also for a session whose Keeper instance died mid-recording — the body up to that point is still complete and replayable.';

COMMENT ON TABLE console_recording_parts IS
    'The asciicast v2 body of a recording, appended in order. Concatenating body ORDER BY seq yields the cast, header excluded (console_recordings.cast_header).';
