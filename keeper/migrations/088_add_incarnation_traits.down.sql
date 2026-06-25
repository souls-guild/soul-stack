-- 088_add_incarnation_traits.down.sql
--
-- Reversible откат релокации Trait per-soul → per-incarnation (R1): снимаем
-- GIN-индекс и колонку `incarnation.traits`. `souls.traits` (087) НЕ затрагивается
-- (эта миграция его и не трогала; projection target остаётся).

DROP INDEX IF EXISTS incarnation_traits_idx;

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS traits;
