-- 091_extend_heralds_type.up.sql
--
-- ADR-052 amendment (extensible Herald channel types): extends the closed-enum
-- `heralds.type` with the types telegram/slack/mattermost/discord/custom (HTTP
-- class) and email (SMTP class). Only-add -- the previous value `webhook`
-- remains valid, existing rows are not affected.
--
-- CHECK `heralds_type_enum` was created by migration 071 (forward-only, not
-- edited in place): DROP + ADD with the extended set.
--
-- Single source of truth for the type set -- the herald.channelDrivers driver
-- registry (+ email), herald.AllHeraldTypes; a guard test checks this CHECK
-- constant against it and against the huma-enum, so the three places don't
-- drift apart.

ALTER TABLE heralds DROP CONSTRAINT heralds_type_enum;
ALTER TABLE heralds ADD CONSTRAINT heralds_type_enum
    CHECK (type IN ('webhook', 'telegram', 'slack', 'mattermost', 'discord', 'custom', 'email'));

COMMENT ON COLUMN heralds.type IS
    'Delivery channel type (closed-enum, heralds_type_enum): webhook/custom -- HTTP with a webhookPayload body; telegram/slack/mattermost/discord -- HTTP messengers; email -- SMTP. Single source of truth -- herald.AllHeraldTypes.';
