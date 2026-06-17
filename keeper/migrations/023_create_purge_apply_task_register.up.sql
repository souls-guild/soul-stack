-- 023_create_purge_apply_task_register.up.sql
--
-- Reaper-правило `purge_apply_task_register` (docs/keeper/reaper.md): удаляет
-- batch register-строк (`apply_task_register`, миграция 022) тех прогонов,
-- чей `apply_runs` уже в ТЕРМИНАЛЬНОМ статусе (`success` / `failed` /
-- `cancelled`) и завершился (`finished_at`) старше `grace_period`.
--
-- Зачем отдельное правило, если FK `ON DELETE CASCADE` уже чистит register
-- вместе со строкой apply_runs (правило `purge_apply_runs`, default 30d)?
--   - `apply_task_register.register_data` — plaintext-JSONB результатов
--     probe-задач, потенциально с секретами (`register: X` + вывод команды).
--     Это ТРАНЗИЕНТНЫЙ run-state: scenario-runner читает его ровно один раз
--     после cross-host barrier-а для рендера `state_changes.sets`, дальше он
--     не нужен. Хранить его все 30d ретеншена apply-истории — лишнее окно
--     plaintext-хранения. Правило снимает register агрессивнее (default 1h
--     после терминала), оставляя сам apply_run для истории/триажа.
--
-- Критерий «терминальный статус + grace» (а НЕ чистый TTL по created_at):
--   - терминальный фильтр гарантирует, что register АКТИВНОГО (`running`)
--     прогона не удаляется НИКОГДА — независимо от его длительности. TTL по
--     created_at снёс бы register долгого running-прогона, чей created_at уже
--     в прошлом, что испортило бы рендер state_changes после барьера.
--   - grace от `finished_at` — запас на edge-case cross-Keeper-роутинга
--     (ADR-002 stateless): RunResult пришёл и apply_run стал терминальным,
--     но run-goroutine на другом инстансе ещё дочитывает register. Default
--     grace заведомо больше времени «барьер → чтение register».
--
-- Возраст считается от `apply_runs.finished_at` (а не от
-- `apply_task_register.created_at`): «прогон завершён N назад» — корректная
-- мера готовности register к удалению. created_at register-строки сдвигается
-- при каждом retry-upsert той же задачи и не отражает завершённость прогона.
--
-- DELETE ... USING join по composite ключу `(apply_id, sid)` к apply_runs;
-- CTE с LIMIT batch_size ограничивает размер транзакции, как и в остальных
-- Reaper-правилах.

CREATE OR REPLACE FUNCTION purge_apply_task_register(grace_period interval, batch_size integer DEFAULT 1000)
RETURNS BIGINT AS $$
DECLARE
    deleted_count BIGINT;
BEGIN
    WITH expired AS (
        SELECT atr.apply_id, atr.sid, atr.task_idx
        FROM apply_task_register atr
        JOIN apply_runs ar
          ON ar.apply_id = atr.apply_id AND ar.sid = atr.sid
        WHERE ar.status IN ('success', 'failed', 'cancelled')
          AND ar.finished_at IS NOT NULL
          AND ar.finished_at < NOW() - grace_period
        ORDER BY ar.finished_at
        LIMIT batch_size
    )
    DELETE FROM apply_task_register t
    USING expired e
    WHERE t.apply_id = e.apply_id AND t.sid = e.sid AND t.task_idx = e.task_idx;

    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    RETURN deleted_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION purge_apply_task_register(interval, integer) IS
    'Удаляет batch apply_task_register-строк прогонов в терминальном статусе (success/failed/cancelled) с finished_at старше grace_period. register активного (running) прогона не трогает. Возвращает количество удалённых строк.';
