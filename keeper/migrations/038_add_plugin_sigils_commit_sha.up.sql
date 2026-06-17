-- 038_add_plugin_sigils_commit_sha.up.sql
--
-- A1-S3: audit-метка происхождения бинаря плагина (ADR-026(g),
-- docs/keeper/plugins.md → Integrity-model). При git-verified-резолве (Вариант A,
-- F-fetch) Keeper резолвит `source`+`ref` в 40-hex `commit_sha` и зачекаучивает
-- именно этот коммит; `commit_sha` фиксирует, ИЗ КАКОГО git-коммита извлечён
-- допущенный бинарь.
--
-- ВНЕ ПОДПИСИ. Подписываемый блок Sigil-а — (namespace, name, ref,
-- binary_sha256, manifest_sha256), DST `soul-stack/sigil/v1` (ADR-026(b)/(c)).
-- `commit_sha` в этот блок НЕ входит и authority целостности НЕ несёт: authority
-- остаётся за sha256 + подписью Keeper-а (инвариант (b) не ослаблен). Это чисто
-- keeper-side audit-поле «происхождение/читаемость», не trust — поэтому оно НЕ
-- участвует в verify-DTO (shared/pluginhost.SigilRecord) и НЕ едет в
-- PluginSigil-транспорт broadcast-а.
--
-- Колонка nullable (аддитивно, forward-only ADR-007):
--   - старые строки реестра (до этой миграции) происхождения не несут → NULL;
--   - Вариант C допусков (operator-asserted ref, без git-verify) → NULL = legacy
--     operator-asserted («бинарь допущен по ручной метке, git-коммит неизвестен»).
-- Заполнение из ResolvedSlot.CommitSHA при git-verified-allow — слайс S4
-- (здесь только колонка, allow-путь не трогается). Прод-записей нет.

ALTER TABLE plugin_sigils
    ADD COLUMN commit_sha TEXT;

COMMENT ON COLUMN plugin_sigils.commit_sha IS
    'Git-commit, из которого резолвлен допущенный бинарь (audit-метка происхождения, ADR-026(g)). ВНЕ подписываемого блока: authority целостности — sha256 + подпись Keeper-а, не commit_sha. Keeper-audit-only (не в SigilRecord verify-DTO, не в PluginSigil-транспорте). NULL = legacy operator-asserted (Вариант C) или строка до миграции 038.';
