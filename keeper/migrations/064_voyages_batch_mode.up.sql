-- 064_voyages_batch_mode.up.sql
--
-- ADR-043 amendment (2026-06-01) → S-W1: batch_mode discriminator for Voyage.
--
-- New nullable column `voyages.batch_mode` (barrier | window). Forward-compat:
-- NULL is treated as 'barrier' (the currently implemented behavior) - existing
-- runs without the field work as before (ADR-012 forward-compat only-add). The
-- NULL->barrier resolution happens on the orchestrator side (voyage.ResolveBatchMode), not as
-- a column default, so "unset" stays distinguishable from an explicit 'barrier' in audit/UI.
--
--   * barrier - sequential Legs (a batch of batch_size), a barrier between batches.
--   * window  - a sliding window (a pool of concurrency workers from a shared queue, without
--     barriers between batches; batch_size is unused, batch_index=0).
--
-- CHECK invariant: batch_mode IS NULL OR batch_mode IN ('barrier', 'window').

ALTER TABLE voyages
    ADD COLUMN batch_mode TEXT;

ALTER TABLE voyages
    ADD CONSTRAINT voyages_batch_mode_valid
        CHECK (batch_mode IS NULL OR batch_mode IN ('barrier', 'window'));

COMMENT ON COLUMN voyages.batch_mode IS
    'Batching mode (ADR-043 amendment 2026-06-01): barrier (Legs + barrier) | window (sliding window). NULL => barrier (forward-compat).';
