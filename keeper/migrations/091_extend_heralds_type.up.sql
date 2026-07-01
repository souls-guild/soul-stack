-- 091_extend_heralds_type.up.sql
--
-- ADR-052 amendment (расширяемые channel-типы Herald, слайс 1): расширение
-- closed-enum `heralds.type` пилотным типом `telegram` (HTTP-класс, auth через
-- vault-ref bot_token_ref внутри config). Only-add — прежнее значение `webhook`
-- остаётся валидным, существующие строки не затрагиваются.
--
-- CHECK `heralds_type_enum` создан миграцией 071 (forward-only, не правится):
-- DROP + ADD с расширенным набором. Следующие слайсы расширят набор остальными
-- типами (slack/mattermost/discord/custom/email) той же схемой.
--
-- Единый источник набора типов — реестр дескрипторов herald.heraldTypeSpecs
-- (herald.AllHeraldTypes); guard-тест сверяет эту CHECK-константу с ним и с
-- huma-enum, чтобы три места не разъехались.

ALTER TABLE heralds DROP CONSTRAINT heralds_type_enum;
ALTER TABLE heralds ADD CONSTRAINT heralds_type_enum
    CHECK (type IN ('webhook', 'telegram'));
