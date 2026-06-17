-- 002_create_purge_audit_old.down.sql
--
-- Откат функции `purge_audit_old` (см. up-миграцию). Подпись
-- `(interval, integer)` должна совпадать с CREATE — иначе DROP
-- не найдёт целевую функцию.

DROP FUNCTION IF EXISTS purge_audit_old(interval, integer);
