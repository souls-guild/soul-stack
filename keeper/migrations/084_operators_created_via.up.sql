-- 084_operators_created_via.up.sql
--
-- ADR-058(d): поле `created_via` реестра operators — «откуда заведён
-- оператор» (bootstrap / user / ldap / oidc / system). Отличается от
-- `auth_method` («чем оператор логинится»): bootstrap-Архонт заведён через
-- `keeper init`, но логинится по jwt; federated-оператор заведён auto-provision-ом
-- (ldap/oidc), system-строка (`archon-system`) посеяна для FK-атрибуции
-- system-инициированных вставок (auto-import push, federated-provision).
--
-- Это поле легализует `created_by_aid IS NULL` у НЕ-bootstrap-строк
-- (`archon-system` + federated-операторы): bootstrap-инвариант (ровно один
-- первый Архонт) переносится с `created_by_aid IS NULL` на
-- `created_via = 'bootstrap'` миграцией 085.
--
-- DEFAULT 'user' — безопасный fallback для существующих строк, заведённых
-- через Operator API (POST /v1/operators).

ALTER TABLE operators ADD COLUMN created_via TEXT NOT NULL DEFAULT 'user';
ALTER TABLE operators ADD CONSTRAINT created_via_valid
    CHECK (created_via IN ('bootstrap','user','ldap','oidc','system'));

-- reconcile существующих строк: первый bootstrap-Архонт имел created_by_aid IS NULL.
UPDATE operators SET created_via = 'bootstrap' WHERE created_by_aid IS NULL;

-- archon-system (если уже посеян прежним путём) — system.
UPDATE operators SET created_via = 'system' WHERE aid = 'archon-system';

COMMENT ON COLUMN operators.created_via IS
    'Источник заведения оператора (ADR-058): bootstrap|user|ldap|oidc|system. Отличается от auth_method (способ логина).';
