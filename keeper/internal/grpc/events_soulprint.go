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

// soulprintMaxSkew — the threshold above which the
// `received_at - collected_at` skew is logged as a warning (ADR-018: "warn in OTel
// when skew > 10 min"). We only honor the log; a separate OTel metric
// will be added in the obs-extension slice.
const soulprintMaxSkew = 10 * time.Minute

// soulprintFactsMarshaler serializes typed_facts into the JSONB column
// `souls.soulprint_facts`. UseProtoNames: keys are proto field names
// (snake_case), so the soulprint projection into the render context (.self.<path>
// in text/template ≡ soulprint.self.<path> in CEL) is a single snake_case canon
// per ADR-018 / templating.md §3.2. jsonName camelCase is not acceptable here —
// it desyncs the template branch from CEL (E2E BUG-A).
var soulprintFactsMarshaler = protojson.MarshalOptions{UseProtoNames: true}

// handleSoulprintReport — handler for the [keeperv1.SoulprintReport] payload
// (M2.4, ADR-018).
//
// Flow:
//  1. Serialize `typed_facts` into JSON (proto → JSONB column
//     `souls.soulprint_facts`).
//  2. UPDATE souls.{soulprint_facts, soulprint_collected_at,
//     soulprint_received_at} via [soul.UpdateSoulprint].
//  3. Log skew as a warning when `received - collected > 10m` (ADR-018).
//  4. Audit `soulprint.received` (meta only — we don't duplicate the facts
//     themselves; they're already in souls — payload carries flags for grepping in the Operator API).
//
// PG-level errors are logged as warnings — the Soul sends SoulprintReport
// periodically (refresh_interval), so a temporary DB outage should not
// break the stream. The Soul will send a new report on the next tick.
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

	// Skew-warn: ADR-018, OTel metric is a separate slice.
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
		// protojson is the only way to preserve proto semantics
		// (default-zero-value vs explicit, nested-message field-numbers).
		// UseProtoNames: JSONB keys are proto field names (snake_case:
		// pkg_mgr/init_system/primary_ip), NOT jsonName camelCase. This is the canon
		// per ADR-018 / templating.md §3.2: the .self.<path> projection in text/template
		// ≡ CEL soulprint.self.<path> — a single snake_case source of truth. Without
		// the flag, composite keys come out camelCase and the template fails on
		// `{{ .self.os.pkg_mgr }}` (E2E BUG-A, nginx run).
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
