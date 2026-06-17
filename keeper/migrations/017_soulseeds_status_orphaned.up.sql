-- 017_soulseeds_status_orphaned.up.sql
--
-- ADR-017 cascade: добавляем статус `orphaned` к enum `soul_seeds.status`
-- для seed-ов хоста, который физически удалён через
-- `core.cloud.provisioned destroyed`.
--
-- Почему отдельный статус, а не `revoked`:
--   * `revoked` — оператор явно отозвал (security-инцидент, compromise).
--     Audit-семантика: «оператор принял решение».
--   * `orphaned` — хост перестал существовать (cloud-destroy). Audit-
--     семантика: «жизненный цикл VM завершился».
--   Перетирать revoked на orphaned нельзя — revoked > orphaned по
--   приоритету (см. cascade-условие WHERE status='active' в provisioned.go).
--
-- Семантика после миграции:
--   * `active`     — текущий выпущенный, ровно один per SID.
--   * `superseded` — заменён ротацией, новый seed уже active.
--   * `expired`    — двинут Жнецом / Vault PKI после not_after.
--   * `revoked`    — оператор отозвал (compromise).
--   * `orphaned`   — хост удалён cascade-ом из `core.cloud.provisioned destroyed`.

ALTER TABLE soul_seeds
    DROP CONSTRAINT soul_seeds_status_valid;

ALTER TABLE soul_seeds
    ADD CONSTRAINT soul_seeds_status_valid
        CHECK (status IN ('active', 'superseded', 'expired', 'revoked', 'orphaned'));
