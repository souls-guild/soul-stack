-- 051_create_push_runs.up.sql
--
-- Реестр push-прогонов (`POST /v1/push/apply`, MCP `keeper.push.apply`):
-- одна строка = один async-прогон Variant C orchestrator-а (keeper/internal/
-- pushorch). Отдельная таблица от `apply_runs` — там per-(apply_id, sid) с
-- pull-семантикой Soul EventStream-а; здесь — per-apply_id с inventory[] и
-- per-host summary в jsonb (push синхронный oneshot, не идёт через
-- apply_runs-барьер).
--
-- Поля:
--   - `apply_id`         — ULID, PK.
--   - `inventory_sids`   — массив SID push-хостов (resolved из request).
--   - `destiny_ref`      — "<name>@<git-ref>" (как в request).
--   - `ssh_provider`     — имя SshProvider из keeper.yml::plugins.ssh_providers[]
--                          ("" — registry-default).
--   - `input`            — input destiny для рендера (jsonb).
--   - `cleanup_stale`    — флаг доп. cleanup на хостах после успеха.
--   - `status`           — терминал прогона (см. CHECK ниже).
--   - `started_at`       — когда orchestrator принял запрос.
--   - `finished_at`      — когда все per-host задачи отработали (NULL пока running).
--   - `started_by_aid`   — Архонт-инициатор (FK на operators).
--   - `started_by_kid`   — KID Keeper-инстанса (для Reaper purge_orphan_push_runs).
--   - `summary`          — per-host map sid → {status, error?, run_status?} (jsonb).
--
-- Статусы (CHECK):
--   pending           — записан, ещё не подхвачен goroutine-ой
--   running           — goroutine начала per-host dispatch
--   success           — все per-host прогоны success
--   failed            — все per-host прогоны failed
--   partial_failed    — часть success, часть failed
--   cancelled         — Reaper purge_orphan_push_runs терминалил (Keeper умер во время прогона)
--
-- Индексы:
--   - status partial-индексы (только in-flight): дёшево по WHERE status IN
--     ('pending','running') — purge_orphan_push_runs и просмотр активных.
--   - started_by_kid — Reaper фильтрует по kid (свой ли инстанс жив).
--
-- FK на operators: ON DELETE SET NULL — историю прогона не теряем при revoke
-- архонта; archon_aid остаётся NULL (audit-trail в audit_log сохранён).

CREATE TABLE push_runs (
    apply_id          TEXT        PRIMARY KEY,
    inventory_sids    TEXT[]      NOT NULL,
    destiny_ref       TEXT        NOT NULL,
    ssh_provider      TEXT,
    input             JSONB       NOT NULL DEFAULT '{}'::jsonb,
    cleanup_stale     BOOLEAN     NOT NULL DEFAULT FALSE,
    status            TEXT        NOT NULL,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    started_by_aid    TEXT,
    started_by_kid    TEXT        NOT NULL,
    summary           JSONB,
    CONSTRAINT push_runs_status_valid CHECK (status IN
        ('pending','running','success','partial_failed','failed','cancelled')),
    CONSTRAINT push_runs_started_by_aid_fk
        FOREIGN KEY (started_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

CREATE INDEX push_runs_status_idx
    ON push_runs (status)
    WHERE status IN ('pending', 'running');

CREATE INDEX push_runs_started_by_kid_idx
    ON push_runs (started_by_kid)
    WHERE status IN ('pending', 'running');

COMMENT ON TABLE push_runs IS
    'Реестр async-прогонов keeper.push (Variant C orchestrator). Одна строка = один POST /v1/push/apply.';
