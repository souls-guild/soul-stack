-- 094_add_providers_fqdn_suffix.up.sql
--
-- FQDN-суффикс провайдера для self-onboard «Вариант T» (ADR-017(h) amendment:
-- keeper задаёт имя VM → FQDN предсказуем → per-VM токен запекается в userdata
-- ДО create).
--
-- Chicken-egg онбординга: SID = FQDN присваивается провайдером ПОСЛЕ create, а
-- userdata формируется ДО. В «Варианте T» Keeper задаёт базовое имя VM-батча
-- (CreateRequest.name) и знает суффикс FQDN провайдера, поэтому предсказывает
-- полный FQDN каждой VM: `<name>-<index>.<fqdn_suffix>` (напр.
-- `redis-0.fedorovstepan2-dev.vm.xc.clv3`). Зная FQDN заранее, keeper выписывает
-- per-VM bootstrap-токены и кладёт их в userdata (общий blob, cloud-init выбирает
-- свой по hostname) — до create, без claim-callback.
--
-- Суффикс — функция namespace+cluster провайдера (WB: `<namespace>.vm.<cluster>`),
-- стабильная для всех VM провайдера, поэтому живёт в Provider-реестре рядом с
-- region/credentials_ref, а не в profile/essence (Provider — authority над тем,
-- где и как называются VM этого провайдера).
--
-- Nullable: не все драйверы формируют FQDN по схеме `<name>.<suffix>` (AWS даёт
-- instance-private-dns, GCP — internal DNS). NULL/пусто → keeper не может
-- предсказать FQDN → self-onboard для этого провайдера недоступен (шаг
-- core.cloud.created с self_onboard: true отдаст понятную ошибку). Ведущая точка
-- — суффикс БЕЗ ведущей точки (keeper склеивает через '.').

ALTER TABLE providers
    ADD COLUMN fqdn_suffix TEXT;

-- Формат суффикса: DNS-labels через точку, без ведущей/замыкающей точки, без
-- underscore (RFC-1035-совместимо, keeper склеит `<name>.<suffix>` в валидный
-- FQDN). NULL допустим (провайдер без предсказуемого FQDN). Пустая строка НЕ
-- допускается (используй NULL — «суффикса нет»), иначе получился бы FQDN с
-- висящей точкой.
ALTER TABLE providers
    ADD CONSTRAINT providers_fqdn_suffix_format
        CHECK (fqdn_suffix IS NULL OR
               fqdn_suffix ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$');

COMMENT ON COLUMN providers.fqdn_suffix IS
    'FQDN-суффикс провайдера (self-onboard Вариант T, ADR-017(h)): keeper предсказывает FQDN VM как <name>-<index>.<fqdn_suffix>. NULL → self-onboard недоступен для провайдера.';
