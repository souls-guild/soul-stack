-- 072_tiding_ephemeral_payload.down.sql
--
-- Откат N1: снос partial-индекса, CHECK-инварианта и четырёх additive-колонок
-- ephemeral/voyage_id/annotations/projection. Возвращает `tidings` к форме 071.

DROP INDEX IF EXISTS tidings_ephemeral_voyage_idx;

ALTER TABLE tidings
    DROP CONSTRAINT IF EXISTS tidings_ephemeral_voyage_consistent;

ALTER TABLE tidings
    DROP COLUMN IF EXISTS projection,
    DROP COLUMN IF EXISTS annotations,
    DROP COLUMN IF EXISTS voyage_id,
    DROP COLUMN IF EXISTS ephemeral;
