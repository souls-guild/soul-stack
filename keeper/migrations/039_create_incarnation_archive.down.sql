-- 039_create_incarnation_archive.down.sql
--
-- Откат S-D3 archive-таблиц. Архив compliance-данных теряется полностью —
-- down предназначен для dev/CI-reset, не для прода (снос архива снесённых
-- incarnation необратим). Зависимостей у этих таблиц нет (нет входящих FK),
-- поэтому простой DROP в любом порядке.

DROP TABLE IF EXISTS state_history_archive;
DROP TABLE IF EXISTS incarnation_archive;
