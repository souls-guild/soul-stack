-- 130_create_operator_passwords.up.sql
--
-- NIM-1051. A local password is a third way for a human to log in, beside LDAP
-- and OIDC: `auth_method = 'local'`, `POST /auth/local/login`. Only operators
-- enrolled with that method log in by password, the same anti-takeover rule the
-- federated methods follow (ADR-058(d)): a `jwt` operator — `archon-system`,
-- a service account, a bootstrap Archon created without `--password-stdin` —
-- never gets a password.
--
-- The hash lives in its own table, not in `operators`, so no read of an
-- operator ever carries it. It is read and written only by
-- keeper/internal/operator/password.go: the login, create, the admin reset,
-- change-my-password and keeper init --password-stdin.
--
-- The composite foreign key on (aid, auth_method) makes the rule a property of
-- the schema as well: a row can only point at an operator whose method is
-- `local`. `auth_method` is never updated, so the key never has to cascade.

ALTER TABLE operators DROP CONSTRAINT auth_method_valid;
ALTER TABLE operators ADD CONSTRAINT auth_method_valid
    CHECK (auth_method IN ('jwt', 'mtls', 'combined', 'ldap', 'oidc', 'local'));

ALTER TABLE operators ADD CONSTRAINT operators_aid_auth_method_key UNIQUE (aid, auth_method);

CREATE TABLE operator_passwords (
    aid         TEXT        PRIMARY KEY,
    auth_method TEXT        NOT NULL DEFAULT 'local',
    hash        TEXT        NOT NULL,
    set_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    set_by_aid  TEXT        REFERENCES operators (aid),
    CONSTRAINT operator_passwords_local_only CHECK (auth_method = 'local'),
    CONSTRAINT operator_passwords_hash_format CHECK (hash LIKE '$argon2id$%'),
    CONSTRAINT operator_passwords_operator_fkey
        FOREIGN KEY (aid, auth_method) REFERENCES operators (aid, auth_method)
);

COMMENT ON TABLE operator_passwords IS
    'argon2id hash of a local operator password (NIM-1051). One row per operator with auth_method=local; never joined into an operator read.';
COMMENT ON COLUMN operator_passwords.set_by_aid IS
    'Who set the current password: the operator itself (change), an administrator (reset), or NULL (keeper init).';
