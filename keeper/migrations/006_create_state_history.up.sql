-- 006_create_state_history.up.sql
--
-- Журнал изменений `incarnation.state` под ADR-009 / ADR-019. Snapshot
-- per-change: при каждом изменении state пишется state_before / state_after
-- + scenario / apply_id / changed_by_aid / at.
--
-- PK = history_id (ULID, тот же формат, что audit_log.audit_id и apply_id).
-- Foreign keys:
--   - incarnation_name → incarnation(name) ON DELETE CASCADE (журнал
--     умирает вместе с incarnation; recovery из неструктурного backup-а).
--   - changed_by_aid   → operators(aid)   ON DELETE SET NULL (история
--     сохраняется при удалении оператора).

CREATE TABLE state_history (
    history_id         TEXT        PRIMARY KEY,
    incarnation_name   TEXT        NOT NULL,
    scenario           TEXT        NOT NULL,
    state_before       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state_after        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    changed_by_aid     TEXT,
    apply_id           TEXT        NOT NULL,
    at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT state_history_incarnation_fk
        FOREIGN KEY (incarnation_name) REFERENCES incarnation (name) ON DELETE CASCADE,
    CONSTRAINT state_history_changed_by_aid_fk
        FOREIGN KEY (changed_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Типовой запрос — лента истории конкретной incarnation в обратном
-- хронологическом порядке (GET /v1/incarnations/{name}/history).
CREATE INDEX state_history_incarnation_at_idx
    ON state_history (incarnation_name, at DESC);

-- Поиск записи по apply_id (опрос статуса async-операции из
-- POST /v1/incarnations).
CREATE INDEX state_history_apply_id_idx
    ON state_history (apply_id);

COMMENT ON TABLE state_history IS
    'Snapshot per-change журнал incarnation.state (ADR-009 / ADR-019).';
