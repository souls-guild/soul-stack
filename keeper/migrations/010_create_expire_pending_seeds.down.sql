-- 010_create_expire_pending_seeds.down.sql
--
-- Откат функции `expire_pending_seeds` (см. up-миграцию). Подпись
-- `(interval, integer)` должна совпадать с CREATE — иначе DROP
-- не найдёт целевую функцию.

DROP FUNCTION IF EXISTS expire_pending_seeds(interval, integer);
