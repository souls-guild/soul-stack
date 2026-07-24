-- 102_rbac_roles_parent_role.up.sql
--
-- ADR-078 J1 - derived roles: rbac_roles.parent_role.
--
-- Column rbac_roles.parent_role (TEXT nullable, self-FK -> rbac_roles(name)) - the
-- role this role is DERIVED from. NULL = a plain role, exactly what every role is
-- today (BACKCOMPAT: existing rows are untouched and keep ADR-047 semantics).
--
-- For a derived role the role's own default_scope (migration 067) changes meaning
-- from "the absolute scope of the role" to "the DELTA that attenuates the parent":
-- effective_scope(role) = effective_scope(parent) AND default_scope(role). With no
-- parent the parent side is the unrestricted top, so the formula collapses back to
-- plain ADR-047 - one rule, no branch. The delta only ever narrows (the scope
-- grammar has no NOT, so conjunction is monotone), which is the structural half of
-- the attenuation invariant "a child can never exceed its parent".
--
-- Resolution of the chain into effective permissions is NOT part of this migration
-- and NOT part of the binary that ships with it: parent_role is stored and loaded,
-- but nothing reads it when a permission is checked yet (NIM-180). That direction
-- is safe - an unresolved parent means the child grants only its own rows, which is
-- narrower than the intended semantics, never wider.
--
-- Three integrity guards, all fail-closed, all in the DB so they hold for EVERY
-- write path (service, API, migrations, manual SQL) rather than only the Go one:
--
--   1. rbac_roles_parent_not_self  - CHECK: a role cannot be its own parent.
--   2. rbac_roles_parent_role_fk   - FK ON DELETE RESTRICT: deleting a role that
--      still has children is REFUSED. Orphans are the escalation case, not a
--      cleanup nuisance: SET NULL would turn the child's delta into an absolute
--      scope and drop the parent's narrowing (a WIDENING = privilege escalation,
--      the same reasoning as migration 100), re-rooting to the grandparent widens
--      by definition, and CASCADE would silently delete membership and could take
--      out the last cluster-admin behind the self-lockout check. The operator
--      re-parents or deletes the children explicitly.
--   3. rbac_roles_parent_chain_guard trigger - cycles and chain depth (below).
--
-- The partial index serves both the FK's child lookup on DELETE (PostgreSQL does
-- not index the referencing side automatically) and the trigger's downward walk.

ALTER TABLE rbac_roles
    ADD COLUMN parent_role TEXT;

ALTER TABLE rbac_roles
    ADD CONSTRAINT rbac_roles_parent_not_self
        CHECK (parent_role IS NULL OR parent_role <> name);

ALTER TABLE rbac_roles
    ADD CONSTRAINT rbac_roles_parent_role_fk
        FOREIGN KEY (parent_role) REFERENCES rbac_roles (name) ON DELETE RESTRICT;

CREATE INDEX rbac_roles_parent_role_idx
    ON rbac_roles (parent_role)
    WHERE parent_role IS NOT NULL;

COMMENT ON COLUMN rbac_roles.parent_role IS
    'ADR-078: the role this role is derived from (self-FK, ON DELETE RESTRICT). NULL = plain role (backcompat, ADR-047 semantics). When set, the role''s own default_scope is the attenuating DELTA: effective_scope = parent effective_scope AND own default_scope. Cycles and chain depth are guarded by rbac_roles_parent_chain_guard.';

-- rbac_roles_parent_chain_guard - the cycle and depth guard for a parent chain.
--
-- Runs BEFORE INSERT OR UPDATE OF parent_role, i.e. exactly on the writes that can
-- change the shape of the role graph. Both checks are needed because either one
-- alone leaves a hole: without the cycle check a chain can close on itself and the
-- resolver would not terminate; without the depth cap a legal chain can grow
-- unboundedly and every snapshot build pays for it.
--
--   Cycle. The stored graph is acyclic by induction (this trigger is the only gate
--   that lets a parent edge in), so the ONLY cycle a single write can create is one
--   running through the row being written. Walking up from the NEW parent and
--   finding NEW.name means NEW is already an ancestor of its own new parent.
--
--   Depth. A chain is counted in ROLES, not edges: a plain role is depth 1, a role
--   with a parent is depth 2. The cap is 4 (at most 3 parent hops), mirroring the
--   scope-nesting cap in keeper/internal/rbac/scope_ast.go (maxScopeDepth) and
--   duplicated there as maxRoleChainDepth - a guard test pins the two together.
--   Re-parenting has to account for BOTH sides: the ancestors above NEW and the
--   subtree that already hangs below it, otherwise moving a deep subtree under a
--   deep parent would slip past a check that only looked upward.
--
-- Both walks are bounded by max_depth + 1 iterations, so a chain corrupted
-- out-of-band (a row written by an older binary, a restore) makes the guard raise
-- rather than spin. A NULL ancestor length means the parent row is not visible yet;
-- that is not this trigger's business - the FK rejects it right after, in the same
-- statement.
CREATE OR REPLACE FUNCTION rbac_roles_parent_chain_guard() RETURNS trigger AS $$
DECLARE
    max_depth    CONSTANT int := 4;
    ancestors    text[];
    ancestor_len int;
    subtree      int;
BEGIN
    IF NEW.parent_role IS NULL THEN
        RETURN NEW;
    END IF;

    -- Self-parent is the degenerate cycle. The CHECK constraint states the same
    -- rule declaratively (and keeps holding if this trigger is ever dropped), but a
    -- BEFORE trigger runs first, so without this branch the operator would get the
    -- generic cycle message instead of the precise one.
    IF NEW.parent_role = NEW.name THEN
        RAISE EXCEPTION 'rbac: role %: parent_role cannot be the role itself', NEW.name
            USING ERRCODE = 'SS001';
    END IF;

    WITH RECURSIVE up(role_name, next_parent, lvl) AS (
        SELECT r.name, r.parent_role, 1
          FROM rbac_roles r
         WHERE r.name = NEW.parent_role
        UNION ALL
        SELECT r.name, r.parent_role, up.lvl + 1
          FROM rbac_roles r
          JOIN up ON r.name = up.next_parent
         WHERE up.lvl <= max_depth + 1
    )
    SELECT array_agg(role_name), max(lvl) INTO ancestors, ancestor_len FROM up;

    IF NEW.name = ANY (COALESCE(ancestors, ARRAY[]::text[])) THEN
        RAISE EXCEPTION
            'rbac: role %: parent_role % would close a cycle (% is already a descendant of %)',
            NEW.name, NEW.parent_role, NEW.parent_role, NEW.name
            USING ERRCODE = 'SS001';
    END IF;

    WITH RECURSIVE down(role_name, lvl) AS (
        SELECT NEW.name, 1
        UNION ALL
        SELECT r.name, down.lvl + 1
          FROM rbac_roles r
          JOIN down ON r.parent_role = down.role_name
         WHERE down.lvl <= max_depth + 1
    )
    SELECT max(lvl) INTO subtree FROM down;

    IF COALESCE(ancestor_len, 0) + COALESCE(subtree, 1) > max_depth THEN
        RAISE EXCEPTION
            'rbac: role %: parent_role % would make the derivation chain % roles deep (max %)',
            NEW.name, NEW.parent_role,
            COALESCE(ancestor_len, 0) + COALESCE(subtree, 1), max_depth
            USING ERRCODE = 'SS002';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER rbac_roles_parent_chain_guard
    BEFORE INSERT OR UPDATE OF parent_role ON rbac_roles
    FOR EACH ROW EXECUTE FUNCTION rbac_roles_parent_chain_guard();
