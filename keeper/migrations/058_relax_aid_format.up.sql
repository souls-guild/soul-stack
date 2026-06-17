-- 058_relax_aid_format.up.sql
--
-- ADR-014 amendment (2026-05-29): ослабление формата AID. Обязательный
-- префикс `archon-` снят; charset расширен до `[a-z0-9._@-]` для
-- email-подобных внешних имён (LDAP/Keycloak auto-provision). Первый
-- символ обязан быть строчной ASCII-буквой или цифрой, общая длина 2..128.
--
-- Charset намеренно узкий и безопасный: нет `/`/`\` (path-traversal),
-- только ASCII-lowercase (нет unicode-двойников и регистра), нет
-- управляющих/кавычек (нет инъекций).
--
-- Forward-only: миграция 003 (CONSTRAINT aid_format `^archon-[a-z0-9-]{1,62}$`)
-- уже применена в проде, не правится. Старые AID вида `archon-<...>`
-- остаются валидными под новым паттерном (начинаются с буквы, charset —
-- надмножество прежнего).
--
-- PG `~` — POSIX ERE. В классе `[a-z0-9._@-]` дефис в конце литерален,
-- точка внутри класса литеральна — экранирование не требуется.

ALTER TABLE operators DROP CONSTRAINT aid_format;
ALTER TABLE operators ADD CONSTRAINT aid_format
    CHECK (aid ~ '^[a-z0-9][a-z0-9._@-]{1,127}$');
