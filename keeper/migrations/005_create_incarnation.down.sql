-- 005_create_incarnation.down.sql
--
-- DROP TABLE incarnation. FK to operators(aid) - ON DELETE SET NULL, does not
-- block. FK from state_history (006) - must be dropped first via
-- 006.down (golang-migrate applies down migrations in reverse order).

DROP TABLE IF EXISTS incarnation;
