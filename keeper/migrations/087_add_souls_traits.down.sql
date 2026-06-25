-- 087_add_souls_traits.down.sql
--
-- Reversible откат ADR-060 read/target пилота: снимаем GIN-индекс и колонку
-- `souls.traits`. `souls.coven` не затрагивается (миграция его и не трогала).

DROP INDEX IF EXISTS souls_traits_idx;

ALTER TABLE souls
    DROP COLUMN IF EXISTS traits;
