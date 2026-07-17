-- 010_create_expire_pending_seeds.down.sql
--
-- Revert of the `expire_pending_seeds` function (see the up migration). The signature
-- `(interval, integer)` must match CREATE - otherwise DROP
-- will not find the target function.

DROP FUNCTION IF EXISTS expire_pending_seeds(interval, integer);
