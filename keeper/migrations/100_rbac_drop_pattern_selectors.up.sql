-- 100_rbac_drop_pattern_selectors.up.sql
--
-- NIM-128 (ADR-047 boolean scope grammar): the pattern-matching selector types
-- `regex=` / `soulprint=` / `state=` are REMOVED from the scope grammar, and the
-- old trait form `trait=key:value` is replaced by `trait.<key>=value`. Pattern
-- matching over host identity now goes through `host matches <glob>` (glob-only).
--
-- This migration is a FAIL-CLOSED data-guard, NOT an auto-scrub. It scans the two
-- RAW-string scope columns -- `rbac_role_permissions.permission` and
-- `rbac_roles.default_scope` (both TEXT, migrations 026/067) -- for any surviving
-- use of a removed selector type. If any row is found the migration ABORTS with a
-- report listing the offending rows, and the operator must remediate manually.
--
-- Why abort instead of silently stripping the selector: dropping a selector from a
-- permission WIDENS it (a scoped `incarnation.list on regex='…'` would become a bare
-- `incarnation.list` = unrestricted), and nulling a `default_scope` that carries a
-- removed type would make the role's bare permissions unrestricted -- both are a
-- privilege ESCALATION. Removal is only safe as a NARROWING (delete the whole
-- permission), which is a deliberate operator decision, so this migration refuses to
-- guess. The correct order of a real upgrade is: run this migration (data clean) ->
-- then roll out the new binary; a binary that no longer knows these selector types
-- would fail its enforcer snapshot build, so the data must be clean first.
--
-- Project is at the design stage with no real installations, so 0 offending rows is
-- expected and the abort is a guard, not a burden. The migration only READS: on a
-- clean database it makes no changes and is trivially idempotent / re-runnable.

DO $$
DECLARE
    n_perms          bigint;
    n_scopes         bigint;
    offending_perms  text;
    offending_scopes text;
BEGIN
    SELECT count(*),
           string_agg(format('(role=%s, permission=%s)', role_name, permission), '; '
                      ORDER BY role_name, permission)
      INTO n_perms, offending_perms
      FROM rbac_role_permissions
     WHERE permission LIKE '%regex=%'
        OR permission LIKE '%regex''%'
        OR permission LIKE '%soulprint=%'
        OR permission LIKE '%soulprint''%'
        OR permission LIKE '%state=%'
        OR permission LIKE '%state''%'
        OR permission LIKE '%trait=%';

    SELECT count(*),
           string_agg(format('(role=%s, default_scope=%s)', name, default_scope), '; '
                      ORDER BY name)
      INTO n_scopes, offending_scopes
      FROM rbac_roles
     WHERE default_scope IS NOT NULL
       AND ( default_scope LIKE '%regex=%'
          OR default_scope LIKE '%regex''%'
          OR default_scope LIKE '%soulprint=%'
          OR default_scope LIKE '%soulprint''%'
          OR default_scope LIKE '%state=%'
          OR default_scope LIKE '%state''%'
          OR default_scope LIKE '%trait=%' );

    IF n_perms > 0 OR n_scopes > 0 THEN
        RAISE EXCEPTION 'NIM-128: % role permissions/scopes use removed selector types (regex/soulprint/state or the old trait=key:value form); manually remediate before upgrading -- delete the whole permission (narrowing is safe, stripping the selector would widen it): permissions=[%]; default_scopes=[%]',
            n_perms + n_scopes,
            COALESCE(offending_perms, ''),
            COALESCE(offending_scopes, '');
    END IF;
END $$;
