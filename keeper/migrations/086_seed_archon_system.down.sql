-- 086_seed_archon_system.down.sql
--
-- Откат 086: удаление системного оператора. Фильтр по created_via='system'
-- защищает от случайного удаления одноимённой строки иной природы.
--
-- ВНИМАНИЕ: FK push_providers.created_by_aid → operators(aid) может удерживать
-- строку, если auto-import уже импортировал провайдеры под этим AID. В чистой
-- down-цепочке порядок миграций снимет зависимые таблицы раньше; прод down не
-- выполняется (forward-only), оставляем DELETE как есть.

DELETE FROM operators WHERE aid = 'archon-system' AND created_via = 'system';
