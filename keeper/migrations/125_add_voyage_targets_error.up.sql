-- 125_add_voyage_targets_error.up.sql
--
-- Why a Voyage target ended without running (NIM-989, NIM-617). A target the
-- Keeper refused before its child run existed -- an incarnation left
-- error_locked by an earlier run, a Soul that is not connected -- used to read
-- `failed` with no back-link, and the cause survived only in the Keeper log.
--
-- error_code is a snake_case token from the set documented in
-- docs/keeper/operator-api/voyages.md; there is deliberately no CHECK, so a
-- new token needs no migration. The set is held by voyage.TargetErrorCodes and
-- a guard test compares it with that document.
-- error_message is a sentence the Keeper composed for an operator, never a raw
-- wrapped error: a dispatch error can quote request input.
--
-- Both are NULL on a target that succeeded, and on one whose child run carries
-- its own reason (a scenario run that failed: follow apply_id).

ALTER TABLE voyage_targets
    ADD COLUMN error_code    TEXT,
    ADD COLUMN error_message TEXT;
