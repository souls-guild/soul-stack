-- 101_add_apply_runs_input.up.sql
--
-- The input column (jsonb NULL) for apply_runs: a masked snapshot of the operator
-- input for the run, shown by the read endpoint
-- GET /v1/incarnations/{name}/runs/{apply_id}. Input is run-invariant (identical
-- across all host/passage rows of one apply_id, like scenario/started_by_aid), so
-- it is written on every row on the dispatch write path.
--
-- Values are masked BEFORE the write (audit.MaskSecrets - the write-path masker:
-- sensitive-by-name keys + vault refs -> ***MASKED***, recursive) so secrets never
-- land in PG (invariant A). NULL for rows from paths without operator input
-- (keeper-side / run-sentinel) and for rows written before this migration
-- (nullable, no backfill).

ALTER TABLE apply_runs
    ADD COLUMN input jsonb;

COMMENT ON COLUMN apply_runs.input IS
    'Masked snapshot of the operator input for the run (audit.MaskSecrets on the write path: sensitive keys + vault refs -> ***MASKED***). Run-invariant, written on every host/passage row. NULL for old rows and input-less paths (keeper-side/sentinel). jsonb object.';
