-- 105_rbac_roles_scope_mode.up.sql
--
-- ADR-078 J6 - derived roles: rbac_roles.scope_mode, the recorded INTENT of a
-- derived role's delta.
--
-- Migration 102 gave a derived role a delta and made the parent's scope cascade
-- into it. What it could not record is which of two opposite intentions the
-- operator had, because both produce a `default_scope` string and nothing else:
--
--   track - "my ceiling is the parent, whatever becomes of it". The delta holds
--           only the ADDED narrowing, so the parent moving carries the child with
--           it. This is the ADR-078(b) contract and the default.
--   pin   - "my ceiling is the parent's scope AS OF NOW". The parent's effective
--           scope is materialized INTO the delta at write time, so a later
--           widening of the parent does not reach this role.
--
-- The difference was visible only by comparing strings, and only by guessing:
-- a child whose delta happens to repeat its parent's predicate looks pinned
-- whether anyone chose that or not - which is exactly what the caller-floor bug
-- of NIM-198 forced operators to write. Storing the intent makes `pin` a
-- server-side property rather than a convention of whichever client wrote the
-- role, and lets a cascade report say which children a parent's change actually
-- reaches.
--
-- NOTHING here changes resolution. effective_scope is still
-- `effective_scope(parent) AND default_scope(role)`, evaluated the same way for
-- both modes - pin differs only in what got written into the delta, so the
-- monotone-AND safety argument of 102 is untouched and a pinned child still
-- NARROWS when its parent narrows. The column never reaches the enforcer
-- snapshot; it is read by the write path and published by the catalog.
--
-- NULL = "no derived-scope intent", which is what a plain role has. The
-- biconditional CHECK keeps the two columns from drifting apart: a role is
-- derived and has a mode, or is plain and has neither. Existing rows are all
-- plain, so they take NULL and no backfill is needed.

ALTER TABLE rbac_roles
    ADD COLUMN scope_mode TEXT;

ALTER TABLE rbac_roles
    ADD CONSTRAINT rbac_roles_scope_mode_values
        CHECK (scope_mode IS NULL OR scope_mode IN ('track', 'pin'));

ALTER TABLE rbac_roles
    ADD CONSTRAINT rbac_roles_scope_mode_needs_parent
        CHECK ((parent_role IS NULL) = (scope_mode IS NULL));

COMMENT ON COLUMN rbac_roles.scope_mode IS
    'ADR-078 J6: how this derived role''s default_scope delta relates to its parent. track = the delta is the added narrowing only, so the parent''s scope cascades in (the default). pin = the parent''s effective scope was materialized into the delta at write time, so a later WIDENING of the parent does not reach this role. NULL iff parent_role IS NULL (a plain role). Resolution is identical for both modes: effective_scope = parent effective_scope AND own default_scope.';
