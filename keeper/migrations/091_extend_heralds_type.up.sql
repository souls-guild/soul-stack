-- 091_extend_heralds_type.up.sql
--
-- ADR-052 amendment (расширяемые channel-типы Herald): расширение closed-enum
-- `heralds.type` типами telegram/slack/mattermost/discord/custom (HTTP-класс) и
-- email (SMTP-класс). Only-add — прежнее значение `webhook` остаётся валидным,
-- существующие строки не затрагиваются.
--
-- CHECK `heralds_type_enum` создан миграцией 071 (forward-only, не правится):
-- DROP + ADD с расширенным набором.
--
-- Единый источник набора типов — реестр драйверов herald.channelDrivers (+ email),
-- herald.AllHeraldTypes; guard-тест сверяет эту CHECK-константу с ним и с huma-enum,
-- чтобы три места не разъехались.

ALTER TABLE heralds DROP CONSTRAINT heralds_type_enum;
ALTER TABLE heralds ADD CONSTRAINT heralds_type_enum
    CHECK (type IN ('webhook', 'telegram', 'slack', 'mattermost', 'discord', 'custom', 'email'));

COMMENT ON COLUMN heralds.type IS
    'Тип канала доставки (closed-enum, heralds_type_enum): webhook/custom — HTTP с телом webhookPayload; telegram/slack/mattermost/discord — HTTP-мессенджеры; email — SMTP. Единый источник — herald.AllHeraldTypes.';
