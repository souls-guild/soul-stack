-- 114_drop_drift_check.up.sql
--
-- NIM-446: the Scry drift-check circuit is removed whole — `POST /v1/incarnations/
-- {name}/check-drift`, the MCP twin, the `scry_background` Reaper rule and the
-- `incarnation.drift_checked` audit event all leave with it. Three things in the
-- database follow the code, and they are three different kinds of change.
--
-- 1. THE COLUMNS (schema). `last_drift_check_at` / `last_drift_summary` were
--    added by migration 050 for the background scan and nothing writes them any
--    more. The partial index `incarnation_last_drift_check_at_idx` dies with its
--    column — PG drops dependent indexes automatically, so naming it here would
--    be noise. This is the only genuinely destructive step: the last scan
--    timestamps are gone and are not recoverable from anything else.
--
-- 2. THE RBAC GRANTS (data, and the reason this migration is not optional).
--    `incarnation.check-drift` leaves the catalog in catalog.go, and the catalog
--    is a CLOSED enum with a fail-closed enforcer: [rbac.NewEnforcerFromSnapshot]
--    errors on the first permission string it cannot parse and [rbac.Holder]
--    refuses to start on a cold load. A single surviving grant would therefore
--    not "lose one permission" — it would take the whole cluster's authorization
--    down at the next keeper start. The rows must go in the same change as the
--    catalog entry. Both the bare form and the scoped form (`... on coven=prod`)
--    are matched. Matching is done on a WHITESPACE-NORMALIZED copy of the
--    string, which 109 did not do: `rbac_role_permissions` stores the RAW text
--    (`rbac/crud.go` says so outright) and `ParsePermission` only trims and
--    splits on read, so `" incarnation.check-drift"` or a double space around
--    ` on ` is a legal stored grant that a literal predicate misses — and a
--    missed grant is precisely the startup outage this block exists to avoid.
--    Migration 109 (NIM-330, `incarnation.update-hosts`) is the precedent for
--    the rest of the shape and this follows it deliberately,
--    including leaving a permission-less role in place: a role with zero rows
--    grants nothing, and its name may still be referenced by rbac_role_operators
--    memberships and by derived roles' parent_role. Deleting the grant is the
--    NARROWING direction and safe on its own terms. The wildcard `incarnation.*`
--    is untouched: it never named this action and expands over whatever the
--    catalog holds at load time, which is now one action shorter.
--
-- 3. THE TIDING SUBSCRIPTIONS (data, softer failure mode). `incarnation.
--    drift_checked` leaves herald's run-scope allow-list. Unlike RBAC this is
--    validated at CRUD only, never on load, so a stale row does NOT block
--    startup — it just stops matching, silently, forever. Worse, an operator
--    could no longer edit their own rule without first removing a type they may
--    not know is dead. So the element is stripped rather than left to rot. A
--    rule that named ONLY drift_checked cannot be stripped — `event_types` is
--    NOT NULL with CHECK cardinality > 0 — and is deleted whole: it is a
--    subscription to an event that will never be emitted again, and an empty
--    subscription is not a legal row. Both outcomes are reported: a vanished
--    notification rule is something an operator must be able to find out about
--    from the migration log rather than from a missing alert.
--
--    The delete predicate is "what remains after stripping is empty", NOT
--    "the array equals exactly [drift_checked]". Nothing dedupes event_types
--    on the write path (`herald.ValidateEventTypes` checks each element
--    independently), so `{drift_checked,drift_checked}` is a storable row. An
--    equality predicate would miss it, the UPDATE below would then strip both
--    copies to `{}`, and the CHECK would abort the migration — which, because
--    keeper applies migrations at startup, leaves the schema_migrations row
--    DIRTY and every subsequent start failing. The same cluster-wide wedge
--    step 2 exists to prevent.
--
-- 4. THE STALE DRY-RUN RUNS (data, and the one step that can otherwise MUTATE
--    HOSTS). A check-drift dispatched one `apply_runs` row per roster host with
--    `status='planned'` and `recipe.dry_run=true`; the Acolyte then built
--    `ApplyRequest{dry_run:true}` from that recipe and Soul called `mod.Plan`
--    instead of `mod.Apply`. `Recipe.DryRun` is gone now and `claim.go` no
--    longer threads it, but `ClaimNext` selects on `status='planned'` alone —
--    it has never looked at the recipe. So a row left behind by a keeper that
--    restarted mid-check would be claimed by the new binary and dispatched as a
--    REAL apply of the converge scenario: a pure-read operation turning into a
--    fleet-wide mutation, days later, with nobody watching. They are cancelled
--    here rather than left to be claimed.
--
-- Idempotent throughout: a re-run matches nothing.

-- 1. Columns (migration 050). The partial index dies with the column.
ALTER TABLE incarnation
    DROP COLUMN IF EXISTS last_drift_summary,
    DROP COLUMN IF EXISTS last_drift_check_at;

-- 2. RBAC grants (catalog.go closed enum, fail-closed enforcer).
DO $$
DECLARE
    affected_roles TEXT[];
    emptied_roles  TEXT[];
BEGIN
    SELECT COALESCE(array_agg(DISTINCT role_name), '{}')
    INTO affected_roles
    FROM rbac_role_permissions
    WHERE regexp_replace(btrim(permission), '\s+', ' ', 'g') = 'incarnation.check-drift'
       OR regexp_replace(btrim(permission), '\s+', ' ', 'g') LIKE 'incarnation.check-drift on %';

    IF array_length(affected_roles, 1) IS NULL THEN
        RETURN;
    END IF;

    DELETE FROM rbac_role_permissions
    WHERE regexp_replace(btrim(permission), '\s+', ' ', 'g') = 'incarnation.check-drift'
       OR regexp_replace(btrim(permission), '\s+', ' ', 'g') LIKE 'incarnation.check-drift on %';

    SELECT COALESCE(array_agg(r), '{}')
    INTO emptied_roles
    FROM unnest(affected_roles) AS r
    WHERE NOT EXISTS (
        SELECT 1 FROM rbac_role_permissions rp WHERE rp.role_name = r
    );

    RAISE NOTICE 'NIM-446: dropped incarnation.check-drift from roles %', affected_roles;

    IF array_length(emptied_roles, 1) IS NOT NULL THEN
        RAISE NOTICE 'NIM-446: these roles now hold NO permissions and grant nothing (kept, they may still carry memberships or derived children): %', emptied_roles;
    END IF;
END $$;

-- 3. Tiding subscriptions on the removed audit event.
DO $$
DECLARE
    deleted_tidings  TEXT[];
    stripped_tidings TEXT[];
BEGIN
    -- Rules left with nothing after stripping (one copy or several): the CHECK
    -- forbids an empty event_types, so the subscription goes whole.
    WITH gone AS (
        DELETE FROM tidings
        WHERE 'incarnation.drift_checked' = ANY (event_types)
          AND cardinality(array_remove(event_types, 'incarnation.drift_checked')) = 0
        RETURNING name
    )
    SELECT COALESCE(array_agg(name), '{}') INTO deleted_tidings FROM gone;

    -- Rules that named it alongside other types keep working on the rest.
    WITH kept AS (
        UPDATE tidings
        SET event_types = array_remove(event_types, 'incarnation.drift_checked'),
            updated_at  = NOW()
        WHERE 'incarnation.drift_checked' = ANY (event_types)
        RETURNING name
    )
    SELECT COALESCE(array_agg(name), '{}') INTO stripped_tidings FROM kept;

    IF array_length(deleted_tidings, 1) IS NOT NULL THEN
        RAISE NOTICE 'NIM-446: DELETED tidings subscribed only to incarnation.drift_checked (they can never fire again): %', deleted_tidings;
    END IF;
    IF array_length(stripped_tidings, 1) IS NOT NULL THEN
        RAISE NOTICE 'NIM-446: removed incarnation.drift_checked from tidings (other event_types keep firing): %', stripped_tidings;
    END IF;
END $$;

-- 4. Stale planned dry-run runs. Cancelling them is the difference between a
-- dead row and a fleet-wide apply nobody asked for: ClaimNext selects on
-- status='planned' alone and the recipe no longer carries dry_run, so the new
-- binary would dispatch a leftover check-drift row as a REAL converge apply.
-- Only `planned` is touched — a row already claimed/dispatched belongs to an
-- Acolyte that read the old recipe, and terminal rows are history.
DO $$
DECLARE
    cancelled_runs INT;
BEGIN
    UPDATE apply_runs
    SET status        = 'cancelled',
        finished_at   = NOW(),
        error_summary = COALESCE(error_summary,
            'NIM-446: cancelled by migration 113 - the drift check that queued this run was removed; re-dispatching it would have applied for real')
    WHERE status = 'planned'
      AND recipe->>'dry_run' = 'true';
    GET DIAGNOSTICS cancelled_runs = ROW_COUNT;

    IF cancelled_runs > 0 THEN
        RAISE NOTICE 'NIM-446: cancelled % planned dry-run apply_runs left by a removed check-drift (they would have dispatched as a REAL apply)', cancelled_runs;
    END IF;
END $$;
