-- 073_tiding_task_selector.up.sql
--
-- ADR-052 §l (task selector for Tiding - the epic "task X"), slice T4-match.
--
-- Extends the `tidings` registry with one additive column (no existing field/
-- contract changes semantics):
--   - task - an optional subscription selector for a SPECIFIC run task by its address
--     (register ∪ id from changed_tasks in the incarnation.run_completed event, ADR-052 §j).
--     NULL = no filter (current behavior). A non-empty value -> the rule matches
--     incarnation.run_completed only if its changed_tasks has an entry with
--     register == task OR id == task (dispatcher.matchTask). "Presence in
--     changed_tasks" = the task changed - the selector is self-sufficient (see ADR-052 §l).
--
-- No CHECK on the format: the address is register/id from a single subscription
-- namespace (snake-case), but this is the SELECTOR's value (what to match), not the task's grammar;
-- matching against the actual changed_tasks is done by the dispatcher, a DB-level format CHECK
-- would be brittle (symmetric with the incarnation/cadence selectors, which also have no CHECK).

ALTER TABLE tidings
    ADD COLUMN task TEXT;

COMMENT ON COLUMN tidings.task IS
    'Optional subscription selector for a specific run task by address register∪id from changed_tasks (ADR-052 §l). NULL = no filter.';
