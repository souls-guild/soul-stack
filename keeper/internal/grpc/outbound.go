package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// tracer для in-process span-ов EventStream-подсистемы. Берёт глобальный
// TracerProvider, поднятый [obs.SetupOTel] в main; при OTel disabled
// провайдер no-op — span-ы бесплатны и код не ветвится (ADR-024 §1.2).
var tracer = otel.Tracer("keeper/grpc")

// Outbound — public Keeper-side API для отправки сообщений в EventStream
// (Keeper → Soul, M2.5 + cluster-mode pub/sub routing).
//
// Реализация делегирует enqueue в [StreamManager] и пишет audit-event для
// дiagnostic-наблюдаемости (`apply.dispatched` / `apply.cancelled`;
// seed-rotation отдельно через [eventstream_seedrotation.go], потому что
// сам выпуск seed-а — keeper-internal операция, а не просто отправка).
//
// Cluster-mode routing: если локальный StreamManager не держит SID,
// Outbound проверяет Redis-lease holder (`soul:<sid>:lock`). holder !=
// self → publish FromKeeper в Redis pub/sub-канал
// `outbound:<sid>`, Keeper-holder получает через подписку и
// форвардит в свой стрим. holder == "" → ErrSoulNotConnected.
// holder == self без локального стрима → lease inconsistency (стрим
// закрылся, lease ещё не освободился) — лог + ErrSoulNotConnected.
//
// При nil-Redis (single-instance / dev-сборка) routing деградирует
// до per-instance: nil lookup → сразу ErrSoulNotConnected.
//
// Caller-ы:
//   - scenario-runner (post-M2.5) — `SendApply` после Keeper-side рендера
//     destiny;
//   - Operator API / admin-flow — `SendCancel` на active apply_id;
//   - seedrotation-handler внутри пакета — `SendSeedRotationReply` после
//     успешной выдачи нового cert.
type Outbound struct {
	manager     *StreamManager
	auditWriter audit.Writer
	logger      *slog.Logger

	redis   *keeperredis.Client
	kid     string
	metrics *GRPCMetrics
}

// OutboundDeps — конструкторские параметры [NewOutbound]. Manager /
// AuditWriter / Logger обязательны; Redis + KID — для cluster-mode
// routing-а (nil-Redis допустим, single-instance fallback).
type OutboundDeps struct {
	Manager     *StreamManager
	AuditWriter audit.Writer
	Logger      *slog.Logger

	// Redis — клиент для проверки SoulLease holder-а и публикации в
	// pub/sub-канал. nil → cluster-mode routing выключен (lookup на
	// текущем Keeper-е — единственный путь).
	Redis *keeperredis.Client
	// KID — идентификатор Keeper-инстанса. Обязателен если Redis != nil
	// (без него невозможны self-фильтрация и определение "это наш
	// lease"). Пустая строка при nil-Redis допустима.
	KID string

	// Metrics — keeper_grpc_*-collectors (ADR-024). nil → dispatch-метрика
	// выключена (nil-safe методы [GRPCMetrics] — no-op). Должен быть тем же
	// дескриптором, что в [EventStreamDeps.Metrics] (один Registry).
	Metrics *GRPCMetrics
}

// NewOutbound собирает Outbound над зарегистрированным [StreamManager].
//
// auditWriter обязателен — Keeper-side операции (apply-dispatch, cancel)
// фиксируются в `audit_log` как факт. logger обязателен. Redis +
// KID — опциональны (nil → cluster-routing отключён).
func NewOutbound(deps OutboundDeps) (*Outbound, error) {
	if deps.Manager == nil {
		return nil, errors.New("grpc: Outbound manager is required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("grpc: Outbound auditWriter is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("grpc: Outbound logger is required")
	}
	if deps.Redis != nil && deps.KID == "" {
		return nil, errors.New("grpc: Outbound KID required when Redis is set")
	}
	return &Outbound{
		manager:     deps.Manager,
		auditWriter: deps.AuditWriter,
		logger:      deps.Logger,
		redis:       deps.Redis,
		kid:         deps.KID,
		metrics:     deps.Metrics,
	}, nil
}

// SendApply кладёт `ApplyRequest` в outbound-channel целевого SID.
//
// PM-decision M2.5(2): thread-safe lookup через RWMutex, не блокируется
// на recv (per-entry chan имеет буфер размером [outboundBufferSize]).
// PM-decision M2.5(1): на полный буфер → drop+log → [ErrOutboundQueueFull].
//
// Audit `apply.dispatched` пишется ТОЛЬКО при успешном enqueue/publish.
// При fail-е caller (scenario-runner) сам решает, что писать как `run.completed`.
func (o *Outbound) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	if req == nil {
		return errors.New("grpc: ApplyRequest is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ApplyRequest{ApplyRequest: req},
	}

	// In-process span на единицу dispatch-а (не на весь long-lived стрим).
	// sid / apply_id — атрибуты для фильтрации трейса (в metric-labels их
	// нельзя — cardinality, ADR-024 §2.2); секретов нет. При OTel disabled
	// tracer no-op — Start/End бесплатны.
	ctx, span := tracer.Start(ctx, "grpc.apply_dispatch",
		trace.WithAttributes(
			attribute.String("sid", sid),
			attribute.String("apply_id", req.GetApplyId()),
		),
	)
	defer span.End()

	// Инжект trace-context span-а grpc.apply_dispatch в ApplyRequest, чтобы
	// Soul поднял apply.run как child (сквозная трасса оператор → Keeper → Soul,
	// ADR-024). Propagator — глобальный composite TraceContext из obs.SetupOTel.
	// req мутируется намеренно: он формируется per-dispatch, общего владельца нет.
	// В cluster-mode req сериализуется в Redis as-is (deliver → PublishOutbound),
	// поэтому trace_context уезжает внутри protobuf-байтов автоматически — отдельной
	// обработки на pub/sub-пути не требуется.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	req.TraceContext = carrier["traceparent"]

	err := o.deliver(ctx, sid, msg, "apply", req.GetApplyId())
	o.metrics.ObserveApplyDispatch(err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "apply dispatch failed")
		return err
	}

	if err := o.auditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyDispatched,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: req.GetApplyId(),
		Payload: map[string]any{
			"sid":         sid,
			"apply_id":    req.GetApplyId(),
			"tasks_count": len(req.GetTasks()),
		},
	}); err != nil {
		// Сообщение уже доставлено — fail audit-а не отменяет dispatch
		// (паттерн идентичен Bootstrap-handler-у).
		o.logger.Warn("outbound: audit apply.dispatched failed (message enqueued)",
			slog.String("sid", sid),
			slog.String("apply_id", req.GetApplyId()),
			slog.Any("error", err),
		)
	}
	return nil
}

// SendErrand кладёт `ErrandRequest` в outbound-channel целевого SID.
//
// LOCAL-ONLY вариант: caller (`errand.Dispatcher`) уже принял решение по
// holder-у lease-а (`ReadSoulLeaseHolder`) и зовёт SendErrand только когда
// holder == self. Поэтому здесь cluster-pub/sub-fallback тот же, что у
// [SendApply] (на случай race-у lease-смены между ReadHolder и Send): если
// локального стрима не оказалось, deliver сам пробросит в Redis. Caller
// получит [ErrSoulNotConnected] при полной неудаче.
//
// Audit-event пишет dispatcher (`errand.invoked`/`completed`/…) — это
// чисто «трубопроводная» функция, как [SendSeedRotationReply] / [SendAugurReply].
func (o *Outbound) SendErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error {
	if req == nil {
		return errors.New("grpc: ErrandRequest is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ErrandRequest{ErrandRequest: req},
	}
	return o.deliver(ctx, sid, msg, "errand", req.GetErrandId())
}

// PublishErrand — REMOTE-ONLY путь Errand-dispatch-а (cross-keeper).
// Используется `errand.Dispatcher` когда holder lease — НЕ наш KID:
// publish прямо в `outbound:<sid>`, holder-Keeper форвардит в свой стрим.
//
// Отдельная функция от SendErrand: чтобы Dispatcher мог явно выбрать
// remote-путь без round-trip-а «попробуем local → fallback на Redis»
// (это делает deliver, но dispatcher хочет короче — local-стрима тут
// быть не должно по lease-семантике). При ошибке publish-а / 0
// subscribers вернёт [ErrSoulNotConnected].
func (o *Outbound) PublishErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error {
	if req == nil {
		return errors.New("grpc: ErrandRequest is nil")
	}
	if o.redis == nil {
		return fmt.Errorf("%w: %s (redis not configured)", ErrSoulNotConnected, sid)
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ErrandRequest{ErrandRequest: req},
	}
	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: errand pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", req.GetErrandId()),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		o.logger.Warn("outbound: errand pub/sub reached zero subscribers",
			slog.String("sid", sid),
			slog.String("errand_id", req.GetErrandId()),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	return nil
}

// SendCancelErrand кладёт `CancelErrand` в outbound-channel целевого SID
// (ADR-033, slice E5).
//
// LOCAL-ONLY вариант: симметрично [SendErrand], caller (`errand.Dispatcher`)
// уже принял решение по holder-у lease-а — здесь deliver сам пробросит в Redis
// pub/sub на случай race-у lease-смены. При полной неудаче — [ErrSoulNotConnected].
//
// Audit-event пишет dispatcher (`errand.cancelled` — отдельный event-type, с
// инициатором AID); это «трубопроводная» функция без audit, паттерн [SendErrand].
func (o *Outbound) SendCancelErrand(ctx context.Context, sid, errandID string) error {
	if errandID == "" {
		return errors.New("grpc: CancelErrand errandID is empty")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelErrand{CancelErrand: &keeperv1.CancelErrand{
			ErrandId: errandID,
		}},
	}
	return o.deliver(ctx, sid, msg, "errand-cancel", errandID)
}

// PublishCancelErrand — REMOTE-ONLY путь cancel-а (cross-keeper, ADR-033
// slice E5). Используется `errand.Dispatcher` когда holder lease — НЕ наш KID:
// publish прямо в `outbound:<sid>`, holder-Keeper форвардит в свой стрим.
//
// Паттерн идентичен [PublishErrand]: 0 subscribers → [ErrSoulNotConnected].
func (o *Outbound) PublishCancelErrand(ctx context.Context, sid, errandID string) error {
	if errandID == "" {
		return errors.New("grpc: CancelErrand errandID is empty")
	}
	if o.redis == nil {
		return fmt.Errorf("%w: %s (redis not configured)", ErrSoulNotConnected, sid)
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelErrand{CancelErrand: &keeperv1.CancelErrand{
			ErrandId: errandID,
		}},
	}
	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: errand-cancel pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		o.logger.Warn("outbound: errand-cancel pub/sub reached zero subscribers",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	return nil
}

// SendCancel кладёт `CancelApply` в outbound-channel.
//
// PM-decision M2.5(3): best-effort signal — Keeper не отслеживает active
// apply_id Soul-side. Soul-side ApplyRunner проверяет ctx.Done()/cancel-
// channel и шлёт `RunResult` со `status: CANCELLED` в ответ.
func (o *Outbound) SendCancel(ctx context.Context, sid string, applyID, reason string) error {
	if applyID == "" {
		return errors.New("grpc: CancelApply applyID is empty")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelApply{CancelApply: &keeperv1.CancelApply{
			ApplyId: applyID,
			Reason:  reason,
		}},
	}
	if err := o.deliver(ctx, sid, msg, "cancel", applyID); err != nil {
		return err
	}

	if err := o.auditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyCancelled,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload: map[string]any{
			"sid":      sid,
			"apply_id": applyID,
			"reason":   reason,
		},
	}); err != nil {
		o.logger.Warn("outbound: audit apply.cancelled failed (message enqueued)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
	}
	return nil
}

// SendSeedRotationReply кладёт `SeedRotationReply` в outbound-channel.
//
// Audit для seed-rotation пишется отдельно в [handleSeedRotationRequest]
// (`soul.seed-rotated`) — Outbound сам не пишет audit для этого канала,
// чтобы не дублировать корреляцию. Чистая «трубопроводная» функция.
func (o *Outbound) SendSeedRotationReply(ctx context.Context, sid string, reply *keeperv1.SeedRotationReply) error {
	if reply == nil {
		return errors.New("grpc: SeedRotationReply is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SeedRotationReply{SeedRotationReply: reply},
	}
	return o.deliver(ctx, sid, msg, "seed-rotation-reply", "")
}

// SendAugurReply кладёт `AugurReply` в outbound-channel целевого SID (ADR-025,
// augur.md §5). Чисто «трубопроводная» функция — audit (`augur.fetch_brokered`
// / `augur.access_denied`) пишет сам augur-handler (см. [handleAugurRequest]),
// потому что решение allow/deny и факт чтения известны там, а не на уровне
// отправки. Паттерн идентичен [SendSeedRotationReply].
func (o *Outbound) SendAugurReply(ctx context.Context, sid string, reply *keeperv1.AugurReply) error {
	if reply == nil {
		return errors.New("grpc: AugurReply is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_AugurReply{AugurReply: reply},
	}
	return o.deliver(ctx, sid, msg, "augur-reply", reply.GetRequestId())
}

// SendSigilSnapshot кладёт `SigilSnapshot` (полный active-набор) в
// outbound-channel целевого SID (ADR-026(h), Вариант A).
//
// Snapshot — единственный авторитетный источник active-набора на Soul-е:
// применяется как ReplaceAll, отсутствующий в наборе допуск Soul забывает
// (near-instant revoke, S6c). Чисто «трубопроводная» функция — audit не пишется
// (выдача/отзыв допуска уже зафиксированы в `audit_log` на `plugin.allow`/
// `plugin.revoke`; раздача — лишь репликация состояния, паттерн идентичен
// [SendSeedRotationReply]).
//
// nil-sigils допустимы (пустой snapshot = «ни один плагин не допущен» — валидное
// состояние, его и надо донести Soul-у, чтобы он стёр старый набор).
func (o *Outbound) SendSigilSnapshot(ctx context.Context, sid string, sigils []*keeperv1.PluginSigil) error {
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilSnapshot{
			SigilSnapshot: &keeperv1.SigilSnapshot{Sigils: sigils},
		},
	}
	return o.deliver(ctx, sid, msg, "sigil-snapshot", "")
}

// RebroadcastSigils раздаёт переданный active-набор печатей доверия каждому
// Soul-у, чей EventStream висит ЛОКАЛЬНО на этом Keeper-инстансе (ADR-026(h),
// S6c), ОДНИМ [keeperv1.SigilSnapshot] на стрим (ReplaceAll).
//
// Вызывается по cluster-wide invalidate-сигналу (`sigil:invalidate`): после
// allow/revoke на любой ноде каждая нода re-broadcast-ит свежий набор своим
// подключённым Soul-ам. Так закрывается near-instant revoke: Soul ReplaceAll-ом
// забывает отозванный допуск, не дожидаясь reconnect-а.
//
// Только локальные стримы (StreamManager.SIDs): cluster-fanout обеспечивает сам
// pub/sub — каждая нода раздаёт СВОИМ Soul-ам. Cluster-mode pub/sub-форвардинг
// через `outbound:<sid>` здесь НЕ задействуется (раздачей чужих Soul-ов
// занимается их собственная нода по тому же invalidate-сигналу).
//
// Устойчиво к per-stream сбоям: drop одного Soul-а (полный буфер / закрытый
// стрим) логируется и НЕ прерывает раздачу остальным — на следующем reconnect-е
// этот Soul возьмёт свежий snapshot connect-time. Возвращает число Soul-ов,
// которым snapshot ушёл успешно (для метрики/лога caller-а).
//
// sigils — полный active-набор; пустой набор тоже шлётся как факт (snapshot с
// пустым sigils[] = ни один допуск не активен → Soul стирает кеш, S6c).
func (o *Outbound) RebroadcastSigils(ctx context.Context, sigils []*keeperv1.PluginSigil) int {
	sids := o.manager.SIDs()
	delivered := 0
	for _, sid := range sids {
		if err := o.SendSigilSnapshot(ctx, sid, sigils); err != nil {
			o.logger.Warn("outbound: sigil snapshot re-broadcast to soul failed — skipping",
				slog.String("sid", sid),
				slog.Any("error", err),
			)
			continue
		}
		delivered++
	}
	o.logger.Debug("outbound: sigil snapshot re-broadcast complete",
		slog.Int("streams", len(sids)),
		slog.Int("delivered", delivered),
		slog.Int("sigils", len(sigils)),
	)
	return delivered
}

// SendSigilTrustAnchors кладёт `SigilTrustAnchors` (полный набор trust-anchor-ов
// подписи Sigil) в outbound-channel целевого SID (ADR-026(h), R3-S6).
//
// Семантика ReplaceAll: набор — единственный авторитетный источник якорей на
// Soul-е, применяется как полная замена holder-а ([pluginhost.AnchorSet.SetAnchors]).
// Якорь вне нового набора Soul «забывает» (retired-ключ перестаёт верифицировать).
// Чисто «трубопроводная» функция — audit не пишется (ротация ключей зафиксирована
// в `audit_log` на rotation-handler-е S7; раздача — лишь репликация набора,
// паттерн идентичен [SendSigilSnapshot]).
//
// nil/пустой pubkeyPEM допустим (пустой набор = «Sigil выключен / якорей нет» —
// валидное состояние; Soul fail-closed по no_trust_anchor защитит). Best-effort
// раздача по локальным стримам — в [RebroadcastTrustAnchors].
func (o *Outbound) SendSigilTrustAnchors(ctx context.Context, sid string, pubkeyPEM []string) error {
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilTrustAnchors{
			SigilTrustAnchors: &keeperv1.SigilTrustAnchors{PubkeyPem: pubkeyPEM},
		},
	}
	return o.deliver(ctx, sid, msg, "sigil-trust-anchors", "")
}

// RebroadcastTrustAnchors раздаёт переданный набор trust-anchor-ов каждому Soul-у,
// чей EventStream висит ЛОКАЛЬНО на этом Keeper-инстансе (ADR-026(h), R3-S6),
// одним [keeperv1.SigilTrustAnchors] на стрим (ReplaceAll).
//
// Вызывается по cluster-wide сигналу `sigil:anchors-changed` (после ротации ключей
// подписи на любой ноде каждая нода re-broadcast-ит свежий набор своим Soul-ам) и
// daemon-watcher-ом «keeper Signer hot-reload». Cluster-fanout обеспечивает сам
// pub/sub — каждая нода раздаёт СВОИМ Soul-ам, чужих не трогает (cluster-mode
// pub/sub-форвардинг через `outbound:<sid>` здесь НЕ задействуется — паттерн
// идентичен [RebroadcastSigils]).
//
// Устойчиво к per-stream сбоям: drop одного Soul-а (полный буфер / закрытый стрим)
// логируется и НЕ прерывает раздачу остальным — на следующем reconnect-е этот Soul
// возьмёт свежий набор connect-time. Возвращает число Soul-ов, которым набор ушёл
// успешно (для метрики/лога caller-а).
//
// pubkeyPEM — полный набор якорей; пустой набор тоже шлётся как факт (пустой =
// «Sigil выключен / якорей нет» → Soul стирает holder, fail-closed verify).
func (o *Outbound) RebroadcastTrustAnchors(ctx context.Context, pubkeyPEM []string) int {
	sids := o.manager.SIDs()
	delivered := 0
	for _, sid := range sids {
		if err := o.SendSigilTrustAnchors(ctx, sid, pubkeyPEM); err != nil {
			o.logger.Warn("outbound: sigil trust-anchors re-broadcast to soul failed — skipping",
				slog.String("sid", sid),
				slog.Any("error", err),
			)
			continue
		}
		delivered++
	}
	o.logger.Debug("outbound: sigil trust-anchors re-broadcast complete",
		slog.Int("streams", len(sids)),
		slog.Int("delivered", delivered),
		slog.Int("anchors", len(pubkeyPEM)),
	)
	return delivered
}

// deliver — общая логика доставки FromKeeper по трём путям:
//
//  1. Локальный StreamManager держит SID → enqueue (выйти на полный
//     буфер → ErrOutboundQueueFull).
//  2. Cluster-mode: Redis-lease holder != self → publish в pub/sub-канал
//     `outbound:<sid>`. Subscribers=0 → лог + ErrSoulNotConnected
//     (никто не слушает — Soul реконнектится).
//  3. Cluster-mode inconsistency: holder == self, но локального стрима
//     нет → лог warn + ErrSoulNotConnected (lease ещё не освободился
//     после disconnect-а).
//  4. Никто не держит → ErrSoulNotConnected.
//
// kind / applyID — для логов и диагностики (apply_id может быть пуст
// для seed-rotation-reply).
func (o *Outbound) deliver(ctx context.Context, sid string, msg *keeperv1.FromKeeper, kind, applyID string) error {
	if entry := o.manager.lookup(sid); entry != nil {
		if !entry.send(msg) {
			o.logger.Warn("outbound: deliver dropped (queue full or closed)",
				slog.String("sid", sid),
				slog.String("kind", kind),
				slog.String("apply_id", applyID),
			)
			return fmt.Errorf("%w: %s", ErrOutboundQueueFull, sid)
		}
		return nil
	}

	if o.redis == nil {
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}

	holder, err := keeperredis.ReadSoulLeaseHolder(ctx, o.redis, sid)
	if err != nil {
		o.logger.Warn("outbound: lease holder lookup failed",
			slog.String("sid", sid),
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if holder == "" {
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if holder == o.kid {
		// Lease наш, но стрима локально нет — disconnect между Recv и
		// lease.Release. Caller получит NotConnected; на следующем
		// reconnect-е Soul возьмёт lease у нас же или у другого Keeper-а.
		o.logger.Warn("outbound: local lease without active stream",
			slog.String("sid", sid),
			slog.String("kid", o.kid),
			slog.String("kind", kind),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}

	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("holder_kid", holder),
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		// Holder есть в lease, но не подписан на канал — race с Unregister-ом
		// на той стороне (стрим закрылся, lease ещё жив TTL/3..TTL секунд).
		// Семантика та же что у NotConnected.
		o.logger.Warn("outbound: pub/sub publish reached zero subscribers",
			slog.String("sid", sid),
			slog.String("holder_kid", holder),
			slog.String("kind", kind),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	o.logger.Debug("outbound: forwarded via pub/sub",
		slog.String("sid", sid),
		slog.String("holder_kid", holder),
		slog.String("kind", kind),
		slog.Int64("subscribers", n),
	)
	return nil
}
