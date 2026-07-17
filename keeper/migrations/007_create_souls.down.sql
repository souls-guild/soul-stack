-- 007_create_souls.down.sql
--
-- DROP TABLE souls. FKs from bootstrap_tokens (008) and soul_seeds (009) must
-- be dropped earlier - golang-migrate applies .down.sql in reverse order,
-- which ensures the correct sequence automatically.

DROP TABLE IF EXISTS souls;
