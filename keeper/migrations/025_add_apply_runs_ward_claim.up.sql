-- 025_add_apply_runs_ward_claim.up.sql
--
-- ADR-027 Phase 0: аддитивная подготовка схемы `apply_runs` под work-queue +
-- claim модель исполнения (Ward-claim). Phase 0 — ТОЛЬКО схема: код исполнения
-- не меняется, старый in-memory run-goroutine-путь (прямой `running` →
-- терминал) продолжает работать без изменений. Новые колонки/статусы после
-- Phase 0 НИКЕМ не пишутся и не читаются (claim-логика, Acolyte-пул, Summons,
-- recovery-скан — Phase 1+).
--
-- Ward (под-опека задания, naming-rules.md): claim задания исполнения —
-- колонки `claim_by_kid` / `claim_at` / `claim_expires_at` / `attempt` +
-- статусы `planned` / `claimed` на `apply_runs`. «Взять Ward» = атомарно
-- захватить planned-задание (`attempt++` — fencing-epoch). Целевая форма
-- задокументирована в storage.md (закоммичено с ADR-027).
--
-- Колонки — все nullable / DEFAULT, чтобы существующие apply_runs-строки
-- мигрировали без ошибок (forward-only, ADR-007):
--   - claim_by_kid     — KID Acolyte-владельца Ward; NULL пока не заклеймлено.
--   - claim_at         — момент захвата Ward; NULL пока не заклеймлено.
--   - claim_expires_at — lease-дедлайн Ward; протухший (< NOW) возвращается
--                        recovery-сканом Reaper-лидера в `planned` (Phase 2).
--   - attempt          — fencing-epoch: инкремент на каждом claim; 0 для строк,
--                        вставленных старым путём (прямой `running`).
--
-- status — CHECK-constraint (НЕ PG-enum, НЕ голый TEXT): расширяется
-- drop+recreate constraint-а (паттерн миграций 016/017). Существующие значения
-- running/success/failed/cancelled (018) + cancel_requested-семантика (024)
-- СОХРАНЕНЫ; добавлены planned/claimed.

ALTER TABLE apply_runs
    ADD COLUMN claim_by_kid     TEXT,
    ADD COLUMN claim_at         TIMESTAMPTZ,
    ADD COLUMN claim_expires_at TIMESTAMPTZ,
    ADD COLUMN attempt          INTEGER NOT NULL DEFAULT 0;

ALTER TABLE apply_runs
    DROP CONSTRAINT apply_runs_status_valid;

ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_status_valid
        CHECK (status IN ('planned', 'claimed', 'running', 'success', 'failed', 'cancelled'));

-- Partial-индекс под claim-скан / recovery-скан (Phase 1+): захват planned
-- (`FOR UPDATE SKIP LOCKED`) и поиск протухших Ward
-- (`status IN ('claimed','running') AND claim_expires_at < NOW`). Заложен в
-- Phase 0 — на текущем пути активные строки только `running` (planned/claimed
-- никем не пишутся), индекс корректен и безвреден, держит схему в целевой
-- форме без отдельной миграции в Phase 1. Терминальные статусы исключены.
CREATE INDEX apply_runs_claim_scan_idx
    ON apply_runs (status, claim_expires_at)
    WHERE status IN ('planned', 'claimed', 'running');

COMMENT ON COLUMN apply_runs.claim_by_kid IS
    'Ward-claim (ADR-027): KID Acolyte-владельца задания; NULL пока не заклеймлено. Phase 0 — не пишется.';
COMMENT ON COLUMN apply_runs.claim_at IS
    'Ward-claim (ADR-027): момент захвата задания; NULL пока не заклеймлено. Phase 0 — не пишется.';
COMMENT ON COLUMN apply_runs.claim_expires_at IS
    'Ward-claim (ADR-027): lease-дедлайн; протухший возвращается recovery-сканом Reaper в planned. Phase 0 — не пишется.';
COMMENT ON COLUMN apply_runs.attempt IS
    'Ward-claim (ADR-027): fencing-epoch, инкремент на каждом claim. 0 для строк старого пути. Phase 0 — не инкрементится.';
