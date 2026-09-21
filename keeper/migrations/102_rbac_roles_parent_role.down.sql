-- 102_rbac_roles_parent_role.down.sql
--
-- Dropping the column takes its CHECK, its self-FK and the partial index with it;
-- the trigger and its function are independent objects and are dropped explicitly,
-- trigger first (it depends on the function).

DROP TRIGGER IF EXISTS rbac_roles_parent_chain_guard ON rbac_roles;

DROP FUNCTION IF EXISTS rbac_roles_parent_chain_guard();

ALTER TABLE rbac_roles
    DROP COLUMN IF EXISTS parent_role;
