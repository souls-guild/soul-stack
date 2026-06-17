-- 007_create_souls.down.sql
--
-- DROP TABLE souls. FK из bootstrap_tokens (008) и soul_seeds (009) должны
-- быть сняты раньше — golang-migrate применяет .down.sql в обратном порядке,
-- что обеспечивает корректную последовательность автоматически.

DROP TABLE IF EXISTS souls;
