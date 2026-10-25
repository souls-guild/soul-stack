-- 128_create_ldap_sync_operators.up.sql
--
-- NIM-1053. Roles from LDAP groups used to be reconciled only at login, so with
-- tokens living up to `auth.jwt.ttl_max` (90 days, NIM-1045) a person removed
-- from a group kept its role all that time. A background job (the `ldapsync` leader,
-- ADR-058 amendment NIM-1053) now re-reads the groups of every `auth_method =
-- ldap` operator once per interval; this table is its memory, one row per
-- operator it has tried.
--
-- `failed_runs` is what makes "LDAP unreachable keeps the roles for one more
-- interval" survive a leader change: the count of consecutive passes that
-- could not read this operator's groups. The second one drops the group roles.
-- An answer from the directory — groups or "no such entry" — resets it.
--
-- The latest `attempted_at` is also the pass clock: a pass reads every LDAP
-- operator and is due once the previous one is an interval old, so a restart
-- or a new leader does not re-run a pass that just happened.
--
-- The row goes with the operator (ON DELETE CASCADE); operators are not deleted
-- today, revoked ones are simply no longer selected.

CREATE TABLE ldap_sync_operators (
    aid          TEXT        PRIMARY KEY REFERENCES operators (aid) ON DELETE CASCADE,
    attempted_at TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    outcome      TEXT        NOT NULL,
    failed_runs  INTEGER     NOT NULL DEFAULT 0,
    error        TEXT,
    CONSTRAINT ldap_sync_operators_outcome_valid
        CHECK (outcome IN ('synced', 'not_found', 'unreachable', 'lockout_refused', 'guarded', 'reconcile_failed')),
    CONSTRAINT ldap_sync_operators_failed_runs_nonnegative CHECK (failed_runs >= 0),
    CONSTRAINT ldap_sync_operators_error_len CHECK (error IS NULL OR char_length(error) <= 1000)
);

CREATE INDEX ldap_sync_operators_attempted_idx ON ldap_sync_operators (attempted_at);

COMMENT ON TABLE ldap_sync_operators IS
    'Background LDAP group sync state, one row per operator (ADR-058 amendment NIM-1053). failed_runs = consecutive passes that could not read the groups; the second drops the group roles.';
