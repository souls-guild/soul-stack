-- 084_operators_created_via.down.sql
--
-- Откат 084: снятие CHECK + DROP колонки. Применяется фреймворком ПОСЛЕ
-- отката 085 (down-цепочка идёт в обратном порядке), поэтому к этому моменту
-- bootstrap-индекс уже возвращён на `created_by_aid IS NULL` — колонка
-- created_via больше не используется ничем.

ALTER TABLE operators DROP CONSTRAINT created_via_valid;
ALTER TABLE operators DROP COLUMN created_via;
