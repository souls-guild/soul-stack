-- 029_add_apply_runs_recipe.up.sql
--
-- ADR-027(c)(f) Phase 1: the Acolyte renders the task for a single host just-in-time
-- at claim, replaying the render steps from the "recipe" in PG instead of from the
-- run-goroutine's memory (which lives only on one instance and doesn't survive
-- cross-Keeper routing / restart). The recipe is what the Acolyte needs in order to
-- repeat the path vault-resolve -> input-validation -> CEL-render ->
-- text/template-render -> ApplyRequest for its own SID. The target form is
-- keeper/internal/applyrun/recipe.go (the Recipe type) and storage.md (committed
-- with ADR-027).
--
-- Invariant A (ADR-027): the recipe carries vault-ref AS-IS (strings), secrets are NOT
-- revealed. Input stores the operator's `incarnation.spec.input` with string
-- `vault:` references; revealed secrets / essence / RenderedTask / ApplyRequest
-- are NOT put into the recipe - the Acolyte resolves them in RAM at claim and hands them to the Soul,
-- they never land in PG.
--
-- Column nullable (additively, forward-only ADR-007): existing rows and the
-- old Insert(running) path carry no recipe -> NULL. The new Acolyte path
-- (planned task under claim) requires a non-NULL recipe - that's a code invariant
-- (claim logic Phase 1.4.2/1.4.3), not a schema one; at the DDL level the column stays
-- nullable so the migration and the old path work without errors.

ALTER TABLE apply_runs
    ADD COLUMN recipe JSONB;

COMMENT ON COLUMN apply_runs.recipe IS
    'Recipe (ADR-027(c)(f)): render instruction for a just-in-time render of the task by the Acolyte at claim (ServiceRef / ScenarioName / Input). Invariant A: Input carries vault-ref as STRINGS, secrets are NOT revealed; essence/RenderedTask/ApplyRequest are NOT put here. NULL for rows from the old Insert(running) path.';
