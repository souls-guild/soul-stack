-- 085_operators_bootstrap_index.up.sql
--
-- ADR-058(d): перенос bootstrap-инварианта (ровно один первый Архонт) с
-- `created_by_aid IS NULL` (миграция 003) на `created_via = 'bootstrap'`
-- (колонка из миграции 084).
--
-- Прежний индекс держал «единственная строка с created_by_aid IS NULL»; это
-- блокировало посев `archon-system` (created_by_aid = NULL) и federated-операторов
-- (auto-provision без оператора-инициатора → created_by_aid = NULL). После
-- переноса NULL у created_by_aid легален для НЕ-bootstrap-строк; единственность
-- гарантируется только для `created_via = 'bootstrap'`.
--
-- CHECK `self_reference_ok` (миграция 003) НЕ трогаем — он остаётся.

DROP INDEX operators_first_archon_idx;
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_via = 'bootstrap';
