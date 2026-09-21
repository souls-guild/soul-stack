-- 110_drop_incarnation_spec_essence.down.sql
--
-- Schema-neutral by construction: the up-migration changes DATA only (it strips
-- one key out of the freeform `incarnation.spec` jsonb), so there is no structure
-- to undo.
--
-- The override is deliberately NOT restored, and it could not be: the key held
-- operator intent with no projection anywhere else in the database, so there is
-- no source to recompute it from. Nor is one needed — the key had no writer, so
-- a keeper rolled back to pre-ADR-0082 code finds `specEssence()` returning nil,
-- which is exactly what it was already returning before the rollback.

SELECT 1;
