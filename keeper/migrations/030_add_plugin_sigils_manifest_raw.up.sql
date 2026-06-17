-- 030_add_plugin_sigils_manifest_raw.up.sql
--
-- M1-storage фикс: персист byte-exact СЫРЫХ байт manifest.yaml, над которыми
-- Keeper ставит подпись Sigil-а (ADR-026, docs/keeper/plugins.md → Integrity-
-- model). До этой колонки Allow подписывал сырые slot.ManifestBytes, но в
-- реестр клал только JSONB-проекцию (manifestYAMLToJSON), а сырые байты
-- выбрасывал — поэтому S6-sender / S6b-verify не могли получить ИМЕННО
-- подписанные байты для PluginSigil.manifest (broadcast).
--
-- manifest_raw — это КАНОН для verify/broadcast: те же байты, что прошли через
-- NormalizeManifestBytes при подписи (S3↔S6-инвариант), едут в
-- PluginSigil.manifest как есть. Колонка manifest (jsonb, миграция 028) —
-- ПРОИЗВОДНАЯ проекция для query/audit (искать по side_effects / capabilities,
-- показывать в UI), НЕ канон: Normalize("{}") != Normalize(""), JSONB-роундтрип
-- не сохраняет байты.
--
-- Колонка nullable (аддитивно, forward-only ADR-007): существующие строки
-- реестра raw НЕ несут → NULL. Новый Allow-путь требует non-NULL manifest_raw —
-- это инвариант кода (Insert-guard: пустой ManifestRaw = баг вызова, корень
-- доверия), не схемы; на уровне DDL колонка остаётся nullable, чтобы миграция
-- старых строк прошла без ошибок.

ALTER TABLE plugin_sigils
    ADD COLUMN manifest_raw BYTEA;

COMMENT ON COLUMN plugin_sigils.manifest_raw IS
    'Byte-exact СЫРЫЕ байты manifest.yaml, подписанные Keeper-ом (КАНОН для S6-verify/broadcast, едет в PluginSigil.manifest). Колонка manifest (jsonb) — производная query/audit-проекция, НЕ канон. NULL для строк, созданных до миграции 030.';
