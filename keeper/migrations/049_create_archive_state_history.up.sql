-- 049_create_archive_state_history.up.sql
--
-- SQL function for the Reaper rule `archive_state_history` (ADR-Q19 retention).
-- Soft-deletes (`archived_at = NOW()`) old active snapshots of `state_history`
-- beyond the N most recent per incarnation, EXCLUDING snapshots of state_schema
-- migration steps (`scenario = 'migration'`, see migrationScenarioLabel in
-- keeper/internal/incarnation/crud.go::writeMigrationHistory / Unlock -
-- the latter writes scenario='unlock'; a migration snapshot is the only one that
-- goes under scenario='migration', which makes the criterion robust).
--
-- Parameters:
--   * keep_last_n      - how many newest active snapshots to keep per
--                        incarnation (by at DESC). default semantics
--                        is set by the runner (default 50).
--   * keep_version_bump - true: migration-step snapshots are NEVER archived,
--                        regardless of keep_last_n. false:
--                        the rule archives them on par with regular ones.
--   * batch             - limit of soft-deleted rows per run (protection against
--                        long UPDATEs when the rule is first enabled on
--                        accumulated history).
--
-- Algorithm:
--   1. The window function `row_number() OVER (PARTITION BY incarnation_name
--      ORDER BY at DESC, history_id ASC)` numbers the active snapshots
--      within each incarnation from 1 (newest) onward. ORDER BY
--      history_id ASC - a stable tie-breaker for equal `at` values (ULID
--      is monotonic - higher = newer, ASC = the higher one on an at-tie stays
--      "higher", i.e. closer to the keep window; a trade-off for determinism).
--   2. The filter rn > keep_last_n - these are the "beyond N" snapshots, candidates for
--      archiving.
--   3. If keep_version_bump = true - additionally exclude rows
--      with scenario='migration' (version-bump snapshots; restorable
--      anchor for ADR-019 migrations).
--   4. LIMIT batch in the subquery - a batch cap so the first run does not
--      take down the DB with one long UPDATE.
--   5. The UPDATE over the subquery PK sets archived_at = NOW().
--      The returned count(*) - affected rows for this batch.

CREATE OR REPLACE FUNCTION archive_state_history(
    keep_last_n        integer,
    keep_version_bump  boolean,
    batch              integer
) RETURNS BIGINT
LANGUAGE sql AS $$
    WITH ranked AS (
        SELECT history_id,
               scenario,
               row_number() OVER (
                   PARTITION BY incarnation_name
                   ORDER BY at DESC, history_id ASC
               ) AS rn
        FROM state_history
        WHERE archived_at IS NULL
    ),
    archived AS (
        UPDATE state_history sh
        SET archived_at = NOW()
        WHERE sh.history_id IN (
            SELECT history_id
            FROM ranked
            WHERE rn > keep_last_n
              AND (NOT keep_version_bump OR scenario <> 'migration')
            ORDER BY rn DESC, history_id ASC
            LIMIT batch
        )
        RETURNING 1
    )
    SELECT count(*) FROM archived;
$$;

COMMENT ON FUNCTION archive_state_history(integer, boolean, integer) IS
    'Reaper archive_state_history (ADR-Q19): soft-delete of active snapshots beyond the N most recent per incarnation, optionally protecting version-bump snapshots (scenario=migration).';
