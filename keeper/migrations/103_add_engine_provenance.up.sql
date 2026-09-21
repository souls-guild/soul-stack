-- 103_add_engine_provenance.up.sql
--
-- Engine provenance (ADR-0076(l)): what a run was actually EXECUTED BY, as
-- opposed to what it executed. A service definition is rendered by keeper and
-- applied by soul-agent, versioned and upgraded independently; six months later
-- the question about an incarnation in a strange state is "which engines
-- produced this", and nothing in state answered it.
--
-- Facts only. Every compatibility gate already stands elsewhere and is
-- untouched: the declared keeper window on the render path (NIM-159,
-- keeper_version_unsupported) and the per-host soul capability set before
-- dispatch (NIM-161, soul_capability_unsupported). Nothing here enforces
-- anything, and nothing here may be read as a gate.
--
-- apply_runs (018, per-host per-Passage) — the two engines of ONE row:
--   keeper_version — the raw build version of the keeper instance that RENDERED
--     this row's tasks. Per ADR-0076(f) the rendering instance is the authority,
--     and during a rolling upgrade the cluster's instances differ: the inline
--     path stamps at Insert (the run goroutine renders), the Acolyte path stamps
--     at claim (the claiming instance renders just-in-time, and a recovery
--     re-claim re-stamps with whoever actually re-rendered).
--   soul_version — the raw Hello.soul_version of the agent the ApplyRequest went
--     to, read at dispatch from the heartbeat Hash field `ver` (NIM-161), which
--     is written by the SAME Hello overwrite as `caps`: the stamped version and
--     the capability set the run was gated against always describe one and the
--     same connection. NULL for keeper-side rows (`on: keeper`) and the run
--     sentinel — no agent was involved.
--
-- incarnation (005) / state_history (006) — engine_compat jsonb, the engine
-- contract in force when the state was produced:
--   {"keeper_version": "<raw>",                     -- the build that rendered
--    "keeper_window": {"min": "…", "max": "…"},     -- EFFECTIVE window: the
--                                                   -- intersection over the
--                                                   -- service manifest and every
--                                                   -- destiny the run resolved
--                                                   -- (ADR-0076(b)); absent =
--                                                   -- nothing was declared
--    "window_enforced": true,                       -- the window was really
--                                                   -- compared: true iff one was
--                                                   -- declared AND the build
--                                                   -- carried a comparable
--                                                   -- version. False therefore
--                                                   -- covers both "nothing
--                                                   -- declared" and the
--                                                   -- version-less build that
--                                                   -- renders unenforced by
--                                                   -- design (ADR-0076(e)) -
--                                                   -- an unchecked run must
--                                                   -- never later read as a
--                                                   -- checked one
--    "soul_capabilities": ["module:core.pkg", …]}   -- union of the required set
--                                                   -- the plan derived per host
--
-- Written ONLY on the successful state commit (UpdateStateFromRun via
-- commitSuccess) — the transition that actually produced the state. The
-- incarnation column uses COALESCE, so a failed run (error_locked, state
-- unchanged) leaves the last successful stamp standing rather than relabelling
-- state it did not produce; state_history rows from paths without a render
-- (unlock, orphan-release, migration) carry NULL.
--
-- Per-entity attribution of the bound is deliberately NOT stored: the effective
-- window is what constrained the render, and which artifact set it is answerable
-- from the recorded refs (incarnation.service_version + the destiny refs pinned
-- in that snapshot, ADR-007). The live per-entity view is served by
-- GET /v1/services/{name}/compat (ADR-0076(h)).
--
-- All four columns are nullable with no backfill: rows written before this
-- migration keep NULL, which reads as "not recorded", never as "version 0".

ALTER TABLE apply_runs
    ADD COLUMN keeper_version TEXT,
    ADD COLUMN soul_version   TEXT;

ALTER TABLE incarnation
    ADD COLUMN engine_compat jsonb;

ALTER TABLE state_history
    ADD COLUMN engine_compat jsonb;

COMMENT ON COLUMN apply_runs.keeper_version IS
    'Raw build version of the keeper instance that RENDERED this row (ADR-0076(l)). Stamped at Insert on the inline path, at claim on the Acolyte path. NULL for rows written before migration 103 and for planned rows not yet claimed.';

COMMENT ON COLUMN apply_runs.soul_version IS
    'Raw Hello.soul_version of the agent this row was dispatched to, read at dispatch from the heartbeat Hash field `ver` (ADR-0076(l)). Audit-only - the gate is capability-based (ADR-0076(n)). NULL for keeper-side rows, the run sentinel, a host that never announced, and rows predating migration 103.';

COMMENT ON COLUMN incarnation.engine_compat IS
    'Engine contract the CURRENT state was produced under (ADR-0076(l)): keeper_version, the effective keeper_window, window_enforced, and the union of required soul_capabilities. Written on the successful state commit only; a failed run leaves the previous stamp. NULL until the first successful run after migration 103.';

COMMENT ON COLUMN state_history.engine_compat IS
    'Engine contract in force at render time for THIS transition (ADR-0076(l)), same shape as incarnation.engine_compat. NULL for transitions without a render (unlock, orphan-release, state migration) and for rows predating migration 103.';
