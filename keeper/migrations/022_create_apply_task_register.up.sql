-- 022_create_apply_task_register.up.sql
--
-- Накопитель register-данных задач прогона (state_changes полная грамматика,
-- слайс 2 «register-в-sets»). Каждая строка — register-результат одной
-- probe-задачи (`register: X`) на одном Soul-хосте в рамках одного прогона.
--
-- Назначение: TaskEvent.register_data доходит до Keeper (events_taskevent.go),
-- но раньше оседал только в audit/SSE и не агрегировался для CEL. Теперь он
-- накапливается тут, а scenario-runner после cross-host barrier-а читает
-- register-map per-host и пробрасывает в RenderStateChanges → `sets:
-- ${ register.<task>.<поле> }` рендерится.
--
-- Хранилище — Postgres (НЕ in-memory): на multi-Keeper (ADR-002 stateless)
-- TaskEvent может прийти на другой инстанс, чем держит run-goroutine. In-memory
-- map собрал бы неполную картину → неверный commit incarnation.state. Общая
-- таблица переживает cross-Keeper-роутинг.
--
-- register_name тут НЕ хранится: handler в момент TaskEvent знает только
-- task_idx (proto register-имя не несёт, ADR-012(d) — orchestrator-only).
-- Резолв task_idx → register_name делает scenario-runner при чтении: он держит
-- []RenderedTask с полем Register на инстансе, инициировавшем прогон.
--
-- PK `(apply_id, sid, task_idx)` — одна строка на (прогон, хост, задача).
-- Повторный TaskEvent той же задачи (retry на Soul-стороне) перезаписывает
-- register_data (upsert ON CONFLICT в store) — побеждает последний результат.
--
-- FK на `apply_runs(apply_id, sid)` ON DELETE CASCADE: register-данные умирают
-- вместе со строкой прогона (Reaper-правило purge_apply_runs чистит их
-- каскадом, default 30d).
--
-- Дополнительно register чистится АГРЕССИВНЕЕ отдельным Reaper-правилом
-- purge_apply_task_register (миграция 023, default grace 1h после терминала
-- apply_run): register_data — plaintext-JSONB с потенциальными секретами,
-- транзиентный run-state, нужный scenario-runner-у только до cross-host
-- barrier-а. Хранить его все 30d ретеншена apply-истории — лишнее окно
-- plaintext-хранения; правило снимает register раньше, оставляя apply_run
-- для истории/триажа.

CREATE TABLE apply_task_register (
    apply_id      TEXT        NOT NULL,
    sid           TEXT        NOT NULL,
    task_idx      INT         NOT NULL,
    register_data JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (apply_id, sid, task_idx),

    CONSTRAINT apply_task_register_apply_run_fk
        FOREIGN KEY (apply_id, sid) REFERENCES apply_runs (apply_id, sid) ON DELETE CASCADE
);

-- Загрузка register-map всего прогона scenario-runner-ом после барьера
-- (per (apply_id) → все хосты и задачи).
CREATE INDEX apply_task_register_apply_idx
    ON apply_task_register (apply_id);

COMMENT ON TABLE apply_task_register IS
    'Накопитель register-данных задач прогона для state_changes.sets (слайс 2). PK (apply_id, sid, task_idx); FK на apply_runs ON DELETE CASCADE.';
