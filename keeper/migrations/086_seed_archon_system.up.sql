-- 086_seed_archon_system.up.sql
--
-- ADR-058(d): посев системного оператора `archon-system` (created_via='system',
-- created_by_aid=NULL). Теперь легально — bootstrap-индекс (миграция 085) держит
-- единственность только для `created_via='bootstrap'`, NULL у created_by_aid вне
-- bootstrap не нарушает инвариант.
--
-- `archon-system` — FK-якорь для system-инициированных вставок:
--   - push auto-import пишет push_providers.created_by_aid = 'archon-system';
--   - federated-provision до ADR-058(d) писал operators.created_by_aid = 'archon-system'
--     (после ADR-058(d) federated пишет NULL + created_via='ldap', но system-строка
--     остаётся якорем для auto-import).
--
-- AID `archon-system` матчит CHECK aid_format (миграция 058: `^[a-z0-9][a-z0-9._@-]{1,127}$`)
-- и прежний паттерн миграции 003 (`^archon-[a-z0-9-]{1,62}$`).
--
-- ON CONFLICT DO NOTHING — идемпотентность: строка могла быть посеяна прежним
-- путём (ручной оператор в pilot S7-4).

INSERT INTO operators (aid, display_name, auth_method, created_by_aid, created_via, metadata)
VALUES ('archon-system', 'System (Soul Stack)', 'jwt', NULL, 'system', '{}'::jsonb)
ON CONFLICT (aid) DO NOTHING;
