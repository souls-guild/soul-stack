-- 005_create_incarnation.down.sql
--
-- DROP TABLE incarnation. FK на operators(aid) — ON DELETE SET NULL, не
-- блокирует. FK из state_history (006) — должен быть снят раньше через
-- 006.down (golang-migrate применяет down-миграции в обратном порядке).

DROP TABLE IF EXISTS incarnation;
