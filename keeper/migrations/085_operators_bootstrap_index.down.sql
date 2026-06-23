-- 085_operators_bootstrap_index.down.sql
--
-- Откат 085: возврат bootstrap-индекса на `created_by_aid IS NULL` (форма из
-- миграции 003). Применяется ДО отката 084 (down-цепочка в обратном порядке),
-- поэтому колонка created_via ещё на месте — DROP/CREATE проходит корректно.
--
-- ВНИМАНИЕ: если в реестре уже есть НЕ-bootstrap-строки с created_by_aid IS NULL
-- (archon-system, federated-операторы), этот откат отвергнет их как нарушение
-- единственности. Down-путь применим только при их отсутствии.

DROP INDEX operators_first_archon_idx;
CREATE UNIQUE INDEX operators_first_archon_idx
    ON operators ((1))
    WHERE created_by_aid IS NULL;
