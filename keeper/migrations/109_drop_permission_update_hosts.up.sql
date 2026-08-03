-- 109_drop_permission_update_hosts.up.sql
--
-- ADR-044 amendment 2026-07-30 (NIM-330): `PATCH /v1/incarnations/{name}/hosts`
-- is unmounted, so the permissions `incarnation.update-hosts` and its deprecated
-- alias `incarnation.update` leave the RBAC catalog (catalog.go).
--
-- This is not cosmetic cleanup. The catalog is a CLOSED enum and the enforcer is
-- fail-closed: [rbac.NewEnforcerFromSnapshot] returns an error on the first
-- permission string it cannot parse, and [rbac.Holder] refuses to start on a cold
-- load. A single surviving row with either name would therefore not "lose one
-- grant" - it would take the whole cluster's authorization down at the next
-- keeper start. The rows must go in the same change as the catalog entries.
--
-- Deleting the grant is the NARROWING direction and is safe on its own terms:
-- migration 100 reached the same conclusion for the removed selector types
-- ("delete the whole permission ... narrowing is safe, stripping the selector
-- would widen it"). We do not RAISE and demand manual remediation the way 100
-- did, because there is nothing for an operator to decide: the endpoint the
-- grant addressed no longer exists in any form.
--
-- Both the bare form and the scoped form (`... on coven=prod`) are matched. The
-- ` on ` separator is pinned by [rbac.ParsePermission], so the two LIKE patterns
-- are exhaustive. Migration 095 matched only the bare form for its own rename and
-- would have missed every scoped grant; that is the bug not repeated here. The
-- space before `on` is what keeps the `incarnation.update` pattern from also
-- matching `incarnation.update-hosts` - they are deleted by separate predicates
-- either way, but the distinction matters if this is ever copied.
--
-- The wildcard grant `incarnation.*` is untouched and needs no fix: it never
-- named these actions, it expands over whatever the catalog holds at load time,
-- and after this change that expansion is simply two actions shorter.
--
-- A role left with no permissions at all is legal (rbac_role_permissions is a
-- side table; a role with zero rows grants nothing) and is reported below rather
-- than deleted - the role name may be referenced by rbac_role_operators
-- memberships and by derived roles' parent_role, and dropping it would cascade
-- far beyond this change.
--
-- Idempotent: a re-run matches nothing.

DO $$
DECLARE
    affected_roles TEXT[];
    emptied_roles  TEXT[];
BEGIN
    SELECT COALESCE(array_agg(DISTINCT role_name), '{}')
    INTO affected_roles
    FROM rbac_role_permissions
    WHERE permission IN ('incarnation.update-hosts', 'incarnation.update')
       OR permission LIKE 'incarnation.update-hosts on %'
       OR permission LIKE 'incarnation.update on %';

    IF array_length(affected_roles, 1) IS NULL THEN
        RETURN;
    END IF;

    DELETE FROM rbac_role_permissions
    WHERE permission IN ('incarnation.update-hosts', 'incarnation.update')
       OR permission LIKE 'incarnation.update-hosts on %'
       OR permission LIKE 'incarnation.update on %';

    SELECT COALESCE(array_agg(r), '{}')
    INTO emptied_roles
    FROM unnest(affected_roles) AS r
    WHERE NOT EXISTS (
        SELECT 1 FROM rbac_role_permissions rp WHERE rp.role_name = r
    );

    RAISE NOTICE 'NIM-330: dropped incarnation.update-hosts / incarnation.update from roles %', affected_roles;

    IF array_length(emptied_roles, 1) IS NOT NULL THEN
        RAISE NOTICE 'NIM-330: these roles now hold NO permissions and grant nothing (kept, they may still carry memberships or derived children): %', emptied_roles;
    END IF;
END $$;
