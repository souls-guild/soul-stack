-- 131_operators_owner.up.sql
--
-- NIM-1057 (ADR-014 amendment). A machine operator — `auth_method = 'jwt'`
-- created through the Operator API — may have an owner: one person
-- (`owner_aid`) or one Synod (`owner_synod`), never both. The owner issues and
-- revokes the operator's tokens under /v1/me/operators without
-- operator.issue-token; keeper/internal/operator/owner.go writes it, and
-- operator.Insert writes the owner a create validated there.
--
-- What the schema holds: at most one owner, never the operator itself, only on
-- a machine operator. What it cannot hold, and the code checks when it writes
-- the owner instead: that an owner Archon logs in itself (local / ldap / oidc)
-- and is not revoked. An owner revoked later stays recorded.
--
-- `owner_synod` has no ON DELETE action on purpose: deleting a Synod that owns
-- an operator fails, and rbac.Service.DeleteSynod answers that with a 409
-- naming the operators before it gets here.

ALTER TABLE operators
    ADD COLUMN owner_aid   TEXT,
    ADD COLUMN owner_synod TEXT;

ALTER TABLE operators
    ADD CONSTRAINT operators_owner_aid_fk
        FOREIGN KEY (owner_aid) REFERENCES operators (aid),
    ADD CONSTRAINT operators_owner_synod_fk
        FOREIGN KEY (owner_synod) REFERENCES synods (name),
    ADD CONSTRAINT operators_owner_one
        CHECK (owner_aid IS NULL OR owner_synod IS NULL),
    ADD CONSTRAINT operators_owner_not_self
        CHECK (owner_aid IS NULL OR owner_aid <> aid),
    ADD CONSTRAINT operators_owner_jwt_only
        CHECK ((owner_aid IS NULL AND owner_synod IS NULL)
               OR (auth_method = 'jwt' AND created_via = 'user'));

CREATE INDEX operators_owner_aid_idx ON operators (owner_aid) WHERE owner_aid IS NOT NULL;
CREATE INDEX operators_owner_synod_idx ON operators (owner_synod) WHERE owner_synod IS NOT NULL;

COMMENT ON COLUMN operators.owner_aid IS
    'The person who owns this machine operator and rotates its tokens (NIM-1057). Exclusive with owner_synod.';
COMMENT ON COLUMN operators.owner_synod IS
    'The Synod whose members own this machine operator and rotate its tokens (NIM-1057): the members who sign in themselves, not a machine operator among them. Exclusive with owner_aid.';
