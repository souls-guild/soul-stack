-- 122_add_apply_runs_dropped.up.sql
--
-- A run that finished without some of its hosts reads `partial_failed`
-- (NIM-936 added the value, NIM-948 produces it). The run status is folded
-- from host rows, and the rows of such a run are the rows of a failed run: a
-- host dropped under `on_failure: host` (docs/scenario/settings.md §4) leaves a
-- `failed` row exactly as a host that ended the run does. This column is what
-- tells the two apart.
--
-- TRUE on the failed row of a host the run dropped, written once the run has
-- reached its end without it. A run that ends early for any reason -- a
-- failure under radius `scenario`, `tolerate` exceeded, every host dropped, a
-- timeout, a Cancel -- marks nothing, so its failed rows still fold to
-- `failed`.

ALTER TABLE apply_runs ADD COLUMN dropped BOOLEAN NOT NULL DEFAULT FALSE;
