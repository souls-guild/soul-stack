-- 127_create_operator_jwts.down.sql
--
-- Drops per-token accounting. Roll the binary back with it: a keeper that knows
-- migration 127 writes a row before it signs any token and reads revoked rows
-- into the RBAC snapshot, so against a database without the table no login,
-- issue-token or snapshot refresh succeeds. Tokens minted with a `jti` keep
-- verifying under the older binary until they expire; their revocations are lost.

DROP FUNCTION IF EXISTS purge_operator_jwts(interval, integer);
DROP TABLE IF EXISTS operator_jwts;
