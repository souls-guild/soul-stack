package grpc

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// soulprintMaxSkew — порог, при котором расхождение
// `received_at - collected_at` логируется warn-ом (ADR-018: "warn в OTel
// при skew > 10 мин"). Соблюдаем только лог, отдельную OTel-метрику
// добавим в slice obs-расширения.
const soulprintMaxSkew = 10 * time.Minute

// soulprintFactsMarshaler сериализует typed_facts в JSONB колонку
// `souls.soulprint_facts`. UseProtoNames: ключи — proto field names
// (snake_case), чтобы проекция soulprint в render-контекст (.self.<path>
// text/template ≡ soulprint.self.<path> CEL) была единым snake_case-каноном
// ADR-018 / templating.md §3.2. jsonName camelCase здесь недопустим —
// это рассинхрон template-ветки с CEL (E2E BUG-A).
var soulprintFactsMarshaler = protojson.MarshalOptions{UseProtoNames: true}

// handleSoulprintReport — обработчик payload-а [keeperv1.SoulprintReport]
// (M2.4, ADR-018).
//
// Поток:
//  1. Сериализуем `typed_facts` в JSON (proto → JSONB колонка
//     `souls.soulprint_facts`).
//  2. UPDATE souls.{soulprint_facts, soulprint_collected_at,
//     soulprint_received_at} через [soul.UpdateSoulprint].
//  3. Логируем skew warn-ом при `received - collected > 10m` (ADR-018).
//  4. Audit `soulprint.received` (только meta — сами facts не дублируем,
//     они уже в souls; payload содержит флаги для grep-а в Operator API).
//
// Ошибки PG-уровня логируются warn-ом — Soul шлёт SoulprintReport
// периодически (refresh_interval), временная недоступность БД не должна
// рвать стрим. Soul на следующем такте пришлёт новый отчёт.
func (h *eventStreamHandler) handleSoulprintReport(ctx context.Context, sid, sessionID string, ev *keeperv1.SoulprintReport) {
	if ev == nil {
		h.logger.Warn("eventstream: SoulprintReport payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	receivedAt := time.Now().UTC()
	var collectedAt time.Time
	if ca := ev.GetCollectedAt(); ca != nil {
		collectedAt = ca.AsTime().UTC()
	}

	// Skew-warn: ADR-018, OTel-метрика — отдельный slice.
	if !collectedAt.IsZero() {
		skew := receivedAt.Sub(collectedAt)
		if skew < 0 {
			skew = -skew
		}
		if skew > soulprintMaxSkew {
			h.logger.Warn("eventstream: soulprint clock skew exceeds threshold",
				slog.String("sid", sid),
				slog.Duration("skew", skew),
				slog.Time("collected_at", collectedAt),
				slog.Time("received_at", receivedAt),
			)
		}
	}

	var factsJSON []byte
	hasTyped := false
	if tf := ev.GetTypedFacts(); tf != nil {
		// protojson — единственный способ сохранить proto-семантику
		// (default-zero-value vs explicit, nested-message field-numbers).
		// UseProtoNames: ключи JSONB — proto field names (snake_case:
		// pkg_mgr/init_system/primary_ip), а НЕ jsonName camelCase. Это канон
		// ADR-018 / templating.md §3.2: проекция .self.<path> в text/template
		// ≡ CEL soulprint.self.<path> — единая точка правды snake_case. Без
		// флага составные ключи приходят camelCase и шаблон падает на
		// `{{ .self.os.pkg_mgr }}` (E2E BUG-A, nginx-прогон).
		b, err := soulprintFactsMarshaler.Marshal(tf)
		if err != nil {
			h.logger.Warn("eventstream: typed_facts marshal failed",
				slog.String("sid", sid), slog.Any("error", err))
		} else {
			factsJSON = b
			hasTyped = true
		}
	}

	if h.deps.SoulDB != nil {
		if err := soul.UpdateSoulprint(ctx, h.deps.SoulDB, sid, factsJSON, collectedAt, receivedAt); err != nil {
			h.logger.Warn("eventstream: soul.UpdateSoulprint failed",
				slog.String("sid", sid), slog.Any("error", err))
		}
	}

	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType: audit.EventSoulprintReceived,
		Source:    audit.SourceSoulGRPC,
		Payload: map[string]any{
			"sid":             sid,
			"collected_at":    collectedAt,
			"received_at":     receivedAt,
			"has_typed_facts": hasTyped,
		},
		CreatedAt: receivedAt,
	}); err != nil {
		h.logger.Warn("eventstream: audit write soulprint.received failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}
