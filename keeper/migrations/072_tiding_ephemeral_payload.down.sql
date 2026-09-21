-- 072_tiding_ephemeral_payload.down.sql
--
-- Rollback of N1: drops the partial index, the CHECK invariant, and the four
-- additive columns ephemeral/voyage_id/annotations/projection. Returns `tidings`
-- to the shape of 071.

DROP INDEX IF EXISTS tidings_ephemeral_voyage_idx;

ALTER TABLE tidings
    DROP CONSTRAINT IF EXISTS tidings_ephemeral_voyage_consistent;

ALTER TABLE tidings
    DROP COLUMN IF EXISTS projection,
    DROP COLUMN IF EXISTS annotations,
    DROP COLUMN IF EXISTS voyage_id,
    DROP COLUMN IF EXISTS ephemeral;
