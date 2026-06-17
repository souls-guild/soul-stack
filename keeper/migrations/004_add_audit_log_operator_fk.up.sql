-- 004_add_audit_log_operator_fk.up.sql
--
-- Доставляет FK `audit_log.archon_aid → operators(aid)`. В 001 поле было
-- объявлено как TEXT nullable без FK (operators ещё не существовала); в
-- 003 таблица operators создана — теперь можно навесить constraint.
--
-- ON DELETE SET NULL: при удалении operator-а audit-записи сохраняются с
-- NULL в archon_aid (история не теряется, но привязка к удалённому
-- Архонту обрывается). Удаление operator-а — редкая операция (обычно
-- используется `revoked_at`), но если случилось — audit-trail важнее
-- ссылочной целостности.

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_archon_aid_fk
    FOREIGN KEY (archon_aid)
    REFERENCES operators (aid)
    ON DELETE SET NULL;
