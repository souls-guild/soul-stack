-- 069_create_synods.down.sql
--
-- Откат Synod-storage (ADR-049). Дочерние таблицы дропаем первыми для явности —
-- ON DELETE CASCADE и сами DROP TABLE сняли бы зависимости в любом порядке.

DROP TABLE IF EXISTS synod_roles;
DROP TABLE IF EXISTS synod_operators;
DROP TABLE IF EXISTS synods;
