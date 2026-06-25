-- 088_add_incarnation_traits.up.sql
--
-- Trait РЕЛОЦИРОВАН per-soul → per-incarnation (ADR-060 amend, R1). До этой
-- миграции Trait жил только на хосте (`souls.traits`, миграция 087) как
-- operator-set-per-soul; пользователь уточнил: traits принадлежат ИНКАРНАЦИИ
-- (организационная метка владельца/продукта/namespace всего инстанса, не
-- отдельного хоста). Эта колонка — НОВЫЙ operator-set источник истины Trait-ов:
-- задаётся в incarnation.spec при create, проецируется МАТЕРИАЛИЗОВАННО в
-- souls.traits хостов-членов через sync-hook (incarnation create + bind хоста
-- через core.soul.registered).
--
-- Зеркало 046 (incarnation.covens): jsonb вместо TEXT[] — значение Trait
-- полиморфно (scalar | list), как и в souls.traits (087); `TEXT[]` этого не
-- выражает. NOT NULL DEFAULT '{}' — incarnation без traits = пустой объект, не
-- NULL (симметрия с covens DEFAULT '{}', read/projection-путь не различает «нет
-- колонки» / «нет меток»).
--
-- souls.traits ОСТАЁТСЯ как projection target (read-слой soulprint.self.traits /
-- where:traits / soul-lint / topology переиспользуется без изменений). Старый
-- per-soul bulk-write (POST /v1/souls/traits) ещё работает на souls.traits
-- напрямую, но в переходный период перетирается проекцией при следующем sync
-- incarnation.traits (relocate per-soul → per-incarnation — следующий слайс).

ALTER TABLE incarnation
    ADD COLUMN traits JSONB NOT NULL DEFAULT '{}'::jsonb;

-- GIN-индекс под таргетинг по incarnation.traits (`traits @> '{"team":"dba"}'`
-- containment) — зеркало souls_traits_idx (087) и параллель GIN по covens[].
-- Поддерживает будущий RBAC-scope-по-traits на incarnation-измерении
-- (разблокирован следующим слайсом), не блокирует R1-фундамент.
CREATE INDEX incarnation_traits_idx
    ON incarnation USING GIN (traits);

COMMENT ON COLUMN incarnation.traits IS
    'Trait — operator-set key-value метки incarnation (ADR-060 amend, R1); значение scalar|list. Источник истины, проецируется в souls.traits хостов-членов через sync-hook.';
