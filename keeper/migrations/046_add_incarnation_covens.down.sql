-- 046_add_incarnation_covens.down.sql

ALTER TABLE incarnation
    DROP COLUMN IF EXISTS covens;
