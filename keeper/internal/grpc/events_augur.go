package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AugurDeps — wire-up зависимости handler-а `AugurRequest` через EventStream
// (ADR-025, augur.md). Брокер (delegate=false, MVP-1): авторизационный резолв +
// чтение Vault KV + отправка AugurReply обратно по тому же стриму.
//
// Все поля обязательны:
//   - DB — реестр omens/rites + souls (резолв Omen / Rite / covens по SID);
//   - Vault — Keeper-side ReadKV (vault-broker + чтение prom/elk-credential по
//     Omen.AuthRef);
//   - Egress — SSRF-guarded HTTP-клиент для prom/elk-брокеров (исходящий HTTP к
//     НЕдоверенному endpoint-у Omen-а, [augur.NewEgressClient]);
//   - AuditWriter — `augur.fetch_brokered` / `augur.access_denied`;
//   - Outbound — Send `AugurReply` обратно в стрим Soul-а.
//
// nil-AugurDeps (handler не wired up) → handler логирует warn и игнорирует
// запрос (минимально-инвазивный fallback на сборках без Augur).
type AugurDeps struct {
	DB          augurDB
	Vault       augur.KVReader
	Egress      augur.HTTPDoer
	AuditWriter audit.Writer
	Outbound    *Outbound

	// Metrics — keeper_augur_*-дескриптор (ADR-024). Опционален: nil →
	// инструментация выключена (nil-safe [augur.BrokerMetrics.ObserveFetch] —
	// no-op), как Metrics в [EventStreamDeps]. Регистрируется в daemon
	// `setupMetricsRegistry`, инжектится здесь же при wire-up брокера.
	Metrics *augur.BrokerMetrics
}

// augurDB — совмещённая поверхность PG, нужная резолву: omens/rites CRUD-ридеры
// (augur.ExecQueryRower) + souls-ридер (soul.ExecQueryRower) для резолва covens
// по SID из авторитетного registry. *pgxpool.Pool удовлетворяет обоим.
type augurDB interface {
	augur.ExecQueryRower
	soul.ExecQueryRower
}

func (d *AugurDeps) validate() error {
	if d.DB == nil {
		return errors.New("grpc: AugurDeps.DB is required")
	}
	if d.Vault == nil {
		return errors.New("grpc: AugurDeps.Vault is required")
	}
	if d.Egress == nil {
		return errors.New("grpc: AugurDeps.Egress is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: AugurDeps.AuditWriter is required")
	}
	if d.Outbound == nil {
		return errors.New("grpc: AugurDeps.Outbound is required")
	}
	return nil
}

// augurOmenReader / augurRiteReader / augurCovenReader — адаптеры реестров под
// узкие reader-интерфейсы [augur.Resolve]. Изолируют enforcement от конкретного
// pool-а и держат covens-резолв на авторитетном souls.coven[] (НЕ из payload).
type augurOmenReader struct{ db augur.ExecQueryRower }

func (r augurOmenReader) OmenByName(ctx context.Context, name string) (*augur.Omen, error) {
	return augur.SelectOmenByName(ctx, r.db, name)
}

type augurRiteReader struct{ db augur.ExecQueryRower }

func (r augurRiteReader) RitesBySubject(ctx context.Context, sid string, covens []string) ([]*augur.Rite, error) {
	return augur.SelectRitesBySubject(ctx, r.db, sid, covens)
}

type augurCovenReader struct{ db soul.ExecQueryRower }

func (r augurCovenReader) CovensBySID(ctx context.Context, sid string) ([]string, error) {
	s, err := soul.SelectBySID(ctx, r.db, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, augur.ErrSubjectUnknown
		}
		return nil, err
	}
	return s.Coven, nil
}

// handleAugurRequest — обработчик payload-а [keeperv1.AugurRequest] (ADR-025).
//
// SID берётся из mTLS peer cert сессии (передан caller-ом), НЕ из
// AugurRequest — авторитет идентичности Soul-а это сертификат (ADR-012(i)).
//
// Запускается в ОТДЕЛЬНОЙ горутине из [dispatch]: fetch (vault/prom/elk) может
// занять время, а receive-loop не должен блокироваться на нём (другие
// FromSoul-ы того же стрима — TaskEvent / RunResult — продолжают приниматься).
// Отмена по ctx.Done() стрима наследуется горутиной.
//
// DoS-guard: спавн горутины проходит через global-семафор augurSem (лимит
// параллельных Augur-обработок поверх ВСЕХ стримов). Non-blocking acquire:
// переполнение → AugurReply{ERROR}, без нового спавна (НЕдоверенный Soul-flood
// AugurRequest-ов не исчерпает горутины/соединения Keeper-а). Семафор
// освобождается в processAugurRequest по любому исходу.
//
// Поток (брокер, delegate=false):
//  1. augur.Resolve (enforcement) — covens из registry, Rite на Omen,
//     query ∈ allow EXACT-match по форме source_type.
//  2. denied → AugurReply{DENIED} + audit `augur.access_denied`.
//  3. allowed → broker по source_type (vault ReadKV / prom HTTP / elk HTTP) →
//     inline_data Struct.
//  4. fetch-сбой → AugurReply{ERROR} (audit не пишем — доступ был разрешён,
//     но fetch не состоялся; это операционный сбой, не security-событие).
//  5. ok → AugurReply{OK, inline_data} + audit `augur.fetch_brokered`.
//
// Секрет/credential НЕ логируется и НЕ попадает в audit: пишутся omen + query +
// request_id, не значение (augur.md §8). Reply — через Outbound.SendAugurReply.
func (h *eventStreamHandler) handleAugurRequest(ctx context.Context, sid, sessionID string, req *keeperv1.AugurRequest) {
	deps := h.deps.Augur
	if deps == nil {
		h.logger.Warn("eventstream: AugurRequest received but augur not wired up",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if req == nil {
		h.logger.Warn("eventstream: AugurRequest payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// Non-blocking acquire: при переполнении не спавним горутину, а сразу шлём
	// ERROR из receive-loop-а (дёшево, без блокировки). nil-семафор (лимит
	// выключен) — старое поведение: спавним всегда.
	if h.augurSem != nil {
		select {
		case h.augurSem <- struct{}{}:
			// слот занят — освобождаем в processAugurRequest.
		default:
			h.logger.Warn("eventstream: augur concurrency limit reached — rejecting request",
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
				slog.String("request_id", req.GetRequestId()),
			)
			// Отбой до резолва — source ещё неизвестен (тип Omen-а не прочитан),
			// исход error (Soul получает AugurReply{ERROR}). Длительность ~0:
			// обработка не стартовала.
			deps.Metrics.ObserveFetch(augur.SourceUnknown, augur.DecisionError, 0)
			h.sendAugurError(ctx, sid, req.GetRequestId(), "augur busy: concurrency limit reached")
			return
		}
		go func() {
			defer func() { <-h.augurSem }()
			h.processAugurRequest(ctx, sid, sessionID, req)
		}()
		return
	}
	go h.processAugurRequest(ctx, sid, sessionID, req)
}

// processAugurRequest — тело обработки (вынесено в отдельную функцию ради
// читаемости горутины). Reply отправляется по любому исходу (OK/DENIED/ERROR);
// status_UNSPECIFIED Soul трактует как DENIED (default-deny на той стороне).
func (h *eventStreamHandler) processAugurRequest(ctx context.Context, sid, sessionID string, req *keeperv1.AugurRequest) {
	deps := h.deps.Augur
	omenName := req.GetOmenName()
	query := req.GetQuery()
	requestID := req.GetRequestId()
	applyID := req.GetApplyId()

	// In-process span на обработку AugurRequest (резолв + fetch). Атрибуты БЕЗ
	// секретов и без cardinality-blow-up (augur.md §8, ADR-024 §2.2): sid —
	// идентичность субъекта (уже в логах/audit), source_type/decision — closed-
	// enum, заполняются по ходу. omen_name / query / значение секрета в span НЕ
	// кладём. При OTel disabled tracer no-op — Start/End бесплатны.
	ctx, span := augur.Tracer().Start(ctx, augur.SpanName,
		trace.WithAttributes(attribute.String("sid", sid)),
	)
	// Метрика и span-статус фиксируются один раз на любом выходе. source/decision
	// заполняются по ходу: до резолва source неизвестен (Omen-тип не прочитан).
	started := time.Now()
	source := augur.SourceUnknown
	decisionLabel := augur.DecisionError
	defer func() {
		deps.Metrics.ObserveFetch(source, decisionLabel, time.Since(started))
		span.SetAttributes(
			attribute.String("source_type", source),
			attribute.String("decision", decisionLabel),
		)
		if decisionLabel == augur.DecisionError {
			span.SetStatus(codes.Error, decisionLabel)
		}
		span.End()
	}()

	decision, err := augur.Resolve(ctx,
		augurOmenReader{db: deps.DB},
		augurRiteReader{db: deps.DB},
		augurCovenReader{db: deps.DB},
		sid, omenName, query,
	)
	if err != nil {
		// Инфраструктурный сбой резолва (PG недоступен) — ERROR, не DENIED.
		// Причину наружу не раскрываем (может нести детали реестра); лог — да.
		h.logger.Warn("eventstream: augur resolve failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.Any("error", err),
		)
		h.sendAugurError(ctx, sid, requestID, "augur resolve failed")
		return
	}

	if !decision.Allowed {
		decisionLabel = augur.DecisionDenied
		// При denied Omen мог не существовать (denied до его чтения) — тогда
		// source остаётся unknown; иначе берём тип найденного Omen-а.
		if decision.Omen != nil {
			source = string(decision.Omen.SourceType)
		}
		h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
			RequestId: requestID,
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_DENIED,
			Error:     decision.Reason,
		})
		h.auditAugur(ctx, audit.EventAugurAccessDenied, sid, omenName, query, requestID, applyID, decision.Reason)
		h.logger.Info("eventstream: augur access denied",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.String("reason", decision.Reason),
		)
		return
	}

	// Доступ разрешён — тип Omen-а известен. source фиксируется здесь, чтобы
	// fetch-сбой ниже учёлся с правильным source (а не unknown).
	source = string(decision.Omen.SourceType)

	// Брокер по source_type. decision.Query — каноническое значение fetch-а:
	// vault — нормализованный logical-path; prom/elk — promQL/index «как есть»
	// (уже прошли exact-match в Resolve). endpoint/auth_ref берутся из
	// decision.Omen (НЕдоверенный endpoint защищён SSRF-guard-ом внутри брокера).
	inline, err := h.brokerFetch(ctx, deps, decision)
	if err != nil {
		// Доступ был разрешён, но fetch не состоялся (внешняя система недоступна
		// / SSRF-guard отверг endpoint / путь исчез) — операционный ERROR, не
		// security-deny. Аудит не пишем (нечего фиксировать как «прочитано»);
		// секрет/credential/тело ответа в ошибку не попадают (см. broker_*.go).
		// Soul-у — обобщённая диагностика.
		h.logger.Warn("eventstream: augur broker fetch failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.String("source_type", string(decision.Omen.SourceType)),
			slog.Any("error", err),
		)
		h.sendAugurError(ctx, sid, requestID, "augur fetch failed")
		return
	}

	decisionLabel = augur.DecisionOK
	h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
		RequestId: requestID,
		Status:    keeperv1.AugurStatus_AUGUR_STATUS_OK,
		Result:    &keeperv1.AugurReply_InlineData{InlineData: inline},
	})
	h.auditAugur(ctx, audit.EventAugurFetchBrokered, sid, omenName, query, requestID, applyID, "")
	h.logger.Info("eventstream: augur fetch brokered",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("omen", omenName),
		slog.String("request_id", requestID),
	)
}

// brokerFetch диспетчит fetch по source_type разрешённого Omen-а (delegate=false,
// augur.md §6 таблица ветвления). vault — ReadKV (Slice B); prometheus / elk —
// SSRF-guarded HTTP к Omen.Endpoint (Slice C). Возвращает inline_data Struct или
// операционную ошибку (caller → AugurReply{ERROR}); ни секрет, ни credential, ни
// тело внешнего ответа в ошибку не попадают.
func (h *eventStreamHandler) brokerFetch(ctx context.Context, deps *AugurDeps, decision *augur.Decision) (*structpb.Struct, error) {
	omen := decision.Omen
	switch omen.SourceType {
	case augur.SourceVault:
		// decision.Query — нормализованный logical-path (прошёл ParseRef в Resolve).
		return augur.BrokerVault(ctx, deps.Vault, decision.Query)
	case augur.SourcePrometheus:
		return augur.BrokerPrometheus(ctx, deps.Vault, deps.Egress, omen.Endpoint, omen.AuthRef, decision.Query)
	case augur.SourceELK:
		return augur.BrokerELK(ctx, deps.Vault, deps.Egress, omen.Endpoint, omen.AuthRef, decision.Query)
	default:
		// Resolve уже отсёк unknown source_type (denied), сюда попасть нельзя без
		// рассинхрона switch-ей — fail-safe.
		return nil, fmt.Errorf("grpc: augur unsupported source_type %q", omen.SourceType)
	}
}

// sendAugurError — отправка AugurReply{ERROR} с обобщённой диагностикой.
func (h *eventStreamHandler) sendAugurError(ctx context.Context, sid, requestID, reason string) {
	h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
		RequestId: requestID,
		Status:    keeperv1.AugurStatus_AUGUR_STATUS_ERROR,
		Error:     reason,
	})
}

// sendAugurReply — отправка reply по тому же стриму через Outbound. Сбой Send-а
// логируется warn-ом (стрим мог закрыться); Soul по таймауту своего ожидания
// перезапросит или провалит шаг — default-deny на той стороне защищает.
func (h *eventStreamHandler) sendAugurReply(ctx context.Context, sid string, reply *keeperv1.AugurReply) {
	if err := h.deps.Augur.Outbound.SendAugurReply(ctx, sid, reply); err != nil {
		h.logger.Warn("eventstream: augur reply send failed",
			slog.String("sid", sid),
			slog.String("request_id", reply.GetRequestId()),
			slog.Any("error", err),
		)
	}
}

// auditAugur пишет augur-событие. Секрет-значение НЕ кладётся (augur.md §8):
// только omen + query + request_id + sid; reason — только для denied. query —
// это vault-логический путь (адрес записи, не значение секрета); augur.md §8
// разрешает логировать путь. MaskSecrets внутри Writer-а ловит лишь литералы с
// префиксом `vault:`, bare-path он НЕ маскирует — и это ОК: путь не секрет.
// Best-effort: fail audit-а не отменяет уже отправленный reply (паттерн
// идентичен прочим event-handler-ам).
func (h *eventStreamHandler) auditAugur(ctx context.Context, evt audit.EventType, sid, omen, query, requestID, applyID, reason string) {
	payload := map[string]any{
		"sid":        sid,
		"omen":       omen,
		"query":      query,
		"request_id": requestID,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := h.deps.Augur.AuditWriter.Write(ctx, &audit.Event{
		EventType:     evt,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: augur audit write failed",
			slog.String("sid", sid),
			slog.String("event", string(evt)),
			slog.Any("error", err),
		)
	}
}
