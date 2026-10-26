-- 131_operators_owner.down.sql
--
-- Drops the owner of a machine operator. Owners are forgotten, nothing else is
-- lost: the tokens an owner minted stay rows of operator_jwts, kind `issued`,
-- created_by_aid the owner. Roll the binary back with it — a keeper that knows
-- owners would read columns that are gone.

DROP INDEX IF EXISTS operators_owner_synod_idx;
DROP INDEX IF EXISTS operators_owner_aid_idx;
ALTER TABLE operators
    DROP CONSTRAINT IF EXISTS operators_owner_jwt_only,
    DROP CONSTRAINT IF EXISTS operators_owner_not_self,
    DROP CONSTRAINT IF EXISTS operators_owner_one,
    DROP CONSTRAINT IF EXISTS operators_owner_synod_fk,
    DROP CONSTRAINT IF EXISTS operators_owner_aid_fk,
    DROP COLUMN IF EXISTS owner_synod,
    DROP COLUMN IF EXISTS owner_aid;
