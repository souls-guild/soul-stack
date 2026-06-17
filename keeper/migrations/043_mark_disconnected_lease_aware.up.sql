-- 043_mark_disconnected_lease_aware.up.sql
--
-- Lease-aware ДВУНАПРАВЛЕННЫЙ reconcile снимка `souls.status` (Reaper-правило
-- `mark_disconnected`, docs/keeper/reaper.md → ADR-006(a) отложенный slice).
-- `souls.status` — это ленивый «последнее известное» снимок для Operator API,
-- НЕ источник presence (online/offline решает Redis SID-lease). Жнец приводит
-- снимок к факту lease-а ФОНОМ в обе стороны:
--
--   • connected → disconnected: stale `last_seen_at` И нет живого SID-lease;
--   • disconnected → connected: жив SID-lease (Soul реально online, реконнект
--     уже-онбордированного Soul-а Bootstrap-RPC не трогает, а eventstream
--     presence в PG на hot-path не пишет — снимок чинит только Жнец).
--
-- Без обратного направления снимок латчился в `disconnected` навсегда после
-- первого «обрыв+sweep»: реконнект lease поднимает, но строку никто не двигал —
-- Operator API отдавал противоречие (status=disconnected + свежий last_seen_at).
--
-- До этой миграции правило метило `disconnected` чисто по PG `last_seen_at`
-- (миграция 014): live-стрим держал `last_seen_at` свежим через throttled-flush
-- из EventStream-handler-а. Но idle-Soul, который шлёт лишь soulprint раз в
-- refresh_interval, мог получить stale `last_seen_at` внутри stale_after и ложно
-- метиться `disconnected` на ЖИВОМ стриме.
--
-- Фикс — двухфазное lease-aware правило в Go (keeper/internal/reaper/purger.go):
--   1) выбрать PG-кандидатов обоих направлений (select_disconnect_candidates /
--      select_reconnect_candidates);
--   2) сверить каждого с Redis SID-lease (Go-сторона, доступ к Redis у Purger-а):
--      нет lease → disconnect, жив lease → reconnect;
--   3) применить (mark_disconnected_sids / mark_connected_sids).
--
-- Старая `mark_disconnected(interval, integer)` (миграция 014) НЕ удаляется:
-- она остаётся fallback-ом, когда Redis не настроен (single-instance dev /
-- unit-режим без координации) — тогда правило одностороннее чисто-SQL, и
-- латча нет по построению (stale `last_seen_at` ⇔ нет стрима на одном инстансе).

-- select_disconnect_candidates — кандидаты на disconnect: connected-souls с
-- `last_seen_at` старше stale_after. Возвращает только SID-ы (Go-сторона
-- сверяет каждый с Redis и метит выжившие). ORDER BY last_seen_at + LIMIT —
-- тот же drain-pattern, что у прочих правил (старейшие первыми, batch-предел).
--
-- last_seen_at IS NULL (никогда не подключавшийся) под предикат не попадает:
-- NULL < NOW() - stale_after = NULL → false (SQL three-valued logic),
-- симметрично исходной mark_disconnected.
CREATE OR REPLACE FUNCTION select_disconnect_candidates(stale_after interval, batch_size integer DEFAULT 1000)
RETURNS SETOF text AS $$
    SELECT sid
    FROM souls
    WHERE status = 'connected'
      AND last_seen_at < NOW() - stale_after
    ORDER BY last_seen_at
    LIMIT batch_size;
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION select_disconnect_candidates(interval, integer) IS
    'Возвращает SID-ы connected-souls со stale last_seen_at (кандидаты mark_disconnected). Go-сторона фильтрует по живому Redis-lease/heartbeat и метит выживших через mark_disconnected_sids.';

-- mark_disconnected_sids — пометить connected → disconnected ровно
-- перечисленные SID-ы. Применяется после Go-фильтрации кандидатов: в списке
-- остаются ТОЛЬКО реально протухшие (без живого Redis-lease/heartbeat).
-- Повторный guard `status = 'connected'` в WHERE — защита от гонки: SID мог
-- сменить статус между select-фазой и mark-фазой (Bootstrap/teardown в другом
-- инстансе). Пустой массив → 0 строк (no-op).
CREATE OR REPLACE FUNCTION mark_disconnected_sids(target_sids text[])
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    UPDATE souls
       SET status = 'disconnected'
     WHERE sid = ANY(target_sids)
       AND status = 'connected';

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_disconnected_sids(text[]) IS
    'Переводит перечисленные connected-souls в disconnected. Применяется после Go-фильтрации кандидатов select_disconnect_candidates по живому Redis-lease/heartbeat. Возвращает количество обновлённых строк.';

-- select_reconnect_candidates — обратное направление reconcile: кандидаты на
-- возврат `disconnected` → `connected`. В отличие от disconnect-направления,
-- БЕЗ предиката по `last_seen_at`: онлайновость определяет живой SID-lease, а не
-- свежесть PG-снимка (idle-Soul на живом стриме держит lease, но `last_seen_at`
-- мог протухнуть — фильтровать его по времени тут нельзя, иначе он бы не вернулся
-- в connected). Возвращает только SID-ы (Go-сторона сверяет каждый с Redis и
-- метит тех, у кого lease ЖИВ). ORDER BY last_seen_at NULLS FIRST + LIMIT — тот же
-- drain-pattern (старейшие/никогда-не-виденные первыми, batch-предел).
CREATE OR REPLACE FUNCTION select_reconnect_candidates(batch_size integer DEFAULT 1000)
RETURNS SETOF text AS $$
    SELECT sid
    FROM souls
    WHERE status = 'disconnected'
    ORDER BY last_seen_at NULLS FIRST
    LIMIT batch_size;
$$ LANGUAGE sql STABLE;

COMMENT ON FUNCTION select_reconnect_candidates(integer) IS
    'Возвращает SID-ы disconnected-souls (любой last_seen_at) — кандидаты на возврат в connected. Go-сторона фильтрует по ЖИВОМУ Redis-lease и метит выживших через mark_connected_sids. Обратное направление reconcile mark_disconnected.';

-- mark_connected_sids — пометить disconnected → connected ровно перечисленные
-- SID-ы. Применяется после Go-фильтрации кандидатов: в списке остаются ТОЛЬКО
-- реально online (с живым Redis SID-lease). Повторный guard `status =
-- 'disconnected'` в WHERE — защита от гонки: SID мог сменить статус между select-
-- и mark-фазой (revoke/teardown/cloud-destroy в другом инстансе) — connected
-- перетирать revoked/destroyed нельзя. Пустой массив → 0 строк (no-op).
CREATE OR REPLACE FUNCTION mark_connected_sids(target_sids text[])
RETURNS BIGINT AS $$
DECLARE
    updated_count BIGINT;
BEGIN
    UPDATE souls
       SET status = 'connected'
     WHERE sid = ANY(target_sids)
       AND status = 'disconnected';

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RETURN updated_count;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION mark_connected_sids(text[]) IS
    'Переводит перечисленные disconnected-souls обратно в connected. Применяется после Go-фильтрации кандидатов select_reconnect_candidates по живому Redis-lease. guard status=disconnected защищает revoked/destroyed от перетирания. Возвращает количество обновлённых строк.';
