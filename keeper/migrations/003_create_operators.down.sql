-- 003_create_operators.down.sql
--
-- Обратная миграция. FK `audit_log.archon_aid → operators(aid)` создаётся
-- в 004 и должен быть снят перед DROP TABLE operators — поэтому 004.down
-- идёт раньше 003.down в порядке отката (`Steps(-N)` golang-migrate
-- применяет .down.sql в обратном порядке номеров, что обеспечивает
-- корректную последовательность автоматически).

DROP TABLE IF EXISTS operators;
