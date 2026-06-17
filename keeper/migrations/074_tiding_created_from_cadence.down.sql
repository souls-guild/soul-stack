-- 074_tiding_created_from_cadence.down.sql
--
-- Откат ADR-052 §m: снос additive-колонки created_from_cadence_id (и её
-- FK/индекса каскадом за DROP COLUMN). Возвращает `tidings` к форме 073.

ALTER TABLE tidings
    DROP COLUMN IF EXISTS created_from_cadence_id;
