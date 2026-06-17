-- 072_tiding_ephemeral_payload.up.sql
--
-- ADR-052 Amendment (2026-06-11, «разовые уведомления + гибкое тело»), слайс N1.
--
-- Расширяет реестр `tidings` четырьмя additive-колонками (ни одно существующее
-- поле/контракт не меняет семантику):
--   - ephemeral   — признак РАЗОВОГО правила, привязанного к одному прогону
--     (ADR-052(g)). Постоянное правило (как в S1) — ephemeral=false, voyage_id NULL.
--   - voyage_id   — селектор привязки к конкретному Voyage (ADR-052(g)). Для
--     ephemeral-правила обязателен (правило матчит ТОЛЬКО события своего прогона);
--     у постоянных правил — NULL.
--   - annotations — статические поля оператора, мержатся новым верхнеуровневым
--     ключом `annotations` в тело webhook-доставки (ADR-052(h)/(i)). JSONB-объект.
--   - projection  — allow-list путей из payload события (ADR-052(h)). Непусто →
--     тело сужается до подмножества; пусто (DEFAULT) = текущая полная форма.
--
-- Merge annotations / projection — в worker-е доставки (off-path, N3); миграция и
-- domain-слой (N1) только ХРАНЯТ поля. voyage_id — TEXT (Voyage.voyage_id), без FK
-- на voyages: ephemeral-Tiding атомарно создаётся keeper-ом в одной tx с Voyage
-- (ADR-052(g) N2), а очистка осиротевших — терминал Voyage + Reaper-TTL, не каскад.

ALTER TABLE tidings
    ADD COLUMN ephemeral   BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN voyage_id   TEXT,
    ADD COLUMN annotations JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN projection   TEXT[]     NOT NULL DEFAULT '{}';

-- Инвариант ephemeral⟺voyage_id (ADR-052(g)): разовое правило привязано к прогону
-- (voyage_id IS NOT NULL), постоянное — без привязки (voyage_id IS NULL). Двойная
-- защита: тот же инвариант проверяет domain-слой (ErrEphemeralRequiresVoyage).
ALTER TABLE tidings
    ADD CONSTRAINT tidings_ephemeral_voyage_consistent
        CHECK (ephemeral = (voyage_id IS NOT NULL));

-- Dispatcher ephemeral-матча (N1) и cleanup осиротевших ephemeral-Tiding (Reaper-
-- TTL, N2) ищут правила по voyage_id. Partial-индекс ТОЛЬКО по ephemeral-строкам:
-- постоянные правила (большинство) индекс не раздувают.
CREATE INDEX tidings_ephemeral_voyage_idx
    ON tidings (voyage_id) WHERE ephemeral;

COMMENT ON COLUMN tidings.ephemeral IS
    'Разовое правило, привязанное к одному прогону (ADR-052(g)). false = постоянное (voyage_id NULL).';
COMMENT ON COLUMN tidings.voyage_id IS
    'Селектор привязки к конкретному Voyage (ADR-052(g)). NOT NULL ⟺ ephemeral.';
COMMENT ON COLUMN tidings.annotations IS
    'Статические поля оператора, мержатся ключом annotations в тело webhook (ADR-052(h)/(i)).';
COMMENT ON COLUMN tidings.projection IS
    'Allow-list путей payload для сужения тела (ADR-052(h)). Пусто = полная форма.';
