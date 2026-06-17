-- 048_state_history_archived_at.down.sql
--
-- Обратимо: дроп partial-индекса и колонки. Soft-deleted-снимки физически
-- остаются в таблице (archived_at дропается, они становятся неотличимы от
-- активных) — это сознательно, чтобы down не уничтожал данные. После
-- down-up цикла активными станут ВСЕ снимки; повторный прогон правила
-- архивирует «лишние» заново.

DROP INDEX IF EXISTS state_history_active_idx;

ALTER TABLE state_history
    DROP COLUMN IF EXISTS archived_at;
