-- 046_add_incarnation_covens.up.sql
--
-- Declared environment-теги incarnation для per-Coven RBAC-scope
-- (ADR-008 amendment a). До этой миграции `coven=`-селектор RBAC на
-- incarnation-эндпоинтах не матчил: extractor клал в context только
-- `{incarnation: name}` без coven/service (docs↔code drift, rbac.md
-- декларировал источник, код не приземлял). Колонка несёт стабильные
-- env-метки (prod/dev/dc1/…), задаваемые оператором при create; RBAC-context
-- incarnation-роутов = `covens ∪ {name}` (имя — корневая Coven-метка по ADR-008).
--
-- Формат каждой метки — CovenPattern (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`),
-- симметрично souls.coven[]. Проверка формата — в API-слое (ValidCoven),
-- не CHECK-constraint: грамматика TEXT[]-элементов в CHECK дороже и дублирует
-- API-валидацию (паттерн souls.coven, у которого CHECK тоже нет).
--
-- DEFAULT '{}' — incarnation без declared env-тегов: coven-scope роли по
-- env не матчит, но `coven=<name>` (имя-как-coven) и `service=` работают.

ALTER TABLE incarnation
    ADD COLUMN covens TEXT[] NOT NULL DEFAULT '{}';

COMMENT ON COLUMN incarnation.covens IS
    'Declared environment-теги incarnation (ADR-008). RBAC-scope coven= для incarnation-операций = covens ∪ {name}.';
