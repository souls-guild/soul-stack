-- 053_add_souls_ssh_target.down.sql

ALTER TABLE souls DROP CONSTRAINT IF EXISTS souls_ssh_target_shape;
ALTER TABLE souls DROP COLUMN IF EXISTS ssh_target;
