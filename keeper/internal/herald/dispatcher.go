package herald

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DefaultRuleCacheTTL is TTL of enabled-Tiding rules snapshot in dispatcher cache.
// Dispatcher does NOT go to PG on each event (hot path of audit write-path):
// keeps rule snapshot, updating it no more than once per TTL (ADR-052(c) —
// "dispatcher matches against enabled rules").
//
// 15s is MVP compromise: Tiding edits rare (CRUD-API appears only in S4),
// 15-second lag of new rule application acceptable. Inline invalidation by
// CRUD hook in same process — [Dispatcher.InvalidateRules] (S4 calls it from
// CRUD handlers). Cross-keeper invalidation (other Keeper created Tiding) —
// open question S4 (Redis pub/sub, pattern RBAC/service-registry
// invalidation); in S2 doesn't block: TTL guarantees convergence in ≤15s.
const DefaultRuleCacheTTL = 15 * time.Second

// RuleSource is source of enabled Tiding rules for dispatcher. In production —
// adapter over CRUD layer (SELECT ... WHERE enabled=true, partial index
// tidings_enabled_idx). Narrow interface for unit-testable matching without
// PG (like ExecQueryRower in CRUD).
type RuleSource interface {
	// EnabledTidings returns current snapshot of ENABLED Tiding rules.
	EnabledTidings(ctx context.Context) ([]*Tiding, error)
}

// Dispatcher matches run audit event against enabled Tiding rules and
// enqueues [DeliveryJob] to [DeliveryQueue] on each match (ADR-052(c), S2 — without
// delivery). Rule snapshot cached with TTL ([DefaultRuleCacheTTL]) — hot
// match path doesn't go to PG.
//
// Thread-safe: Dispatch called from tap goroutine (single), but cache under
// RWMutex for parallel InvalidateRules from CRUD handlers (S4).
type Dispatcher struct {
	source RuleSource
	queue  DeliveryQueue
	logger *slog.Logger
	ttl    time.Duration
	clock  func() time.Time

	mu        sync.RWMutex
	cached    []*Tiding
	cachedAt  time.Time
	cacheInit bool

	metrics *DispatcherMetrics
}

// DispatcherConfig are [Dispatcher] construction parameters.
type DispatcherConfig struct {
	Source RuleSource
	Queue  DeliveryQueue
	Logger *slog.Logger
	// TTL of rule snapshot; <= 0 → [DefaultRuleCacheTTL].
	TTL time.Duration
}

// NewDispatcher constructs Dispatcher. Source and Queue are required.
func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultRuleCacheTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{
		source: cfg.Source,
		queue:  cfg.Queue,
		logger: logger,
		ttl:    ttl,
		clock:  time.Now,
	}
}

// SetQueue does late-binding substitution of delivery queue. Init-order of keeper:
// tap + dispatcher built in setupAudit (with fallback LogDeliveryQueue), Redis
// starts later (setupRedis); real [RedisDeliveryQueue] passed here after — with same
// late-binding technique as SetMetrics. Under cache write-lock (same mu) — Dispatch
// reads queue without separate sync, so substitution serialized with rules cache.
// nil-receiver / nil-queue → no-op.
func (d *Dispatcher) SetQueue(q DeliveryQueue) {
	if d == nil || q == nil {
		return
	}
	d.mu.Lock()
	d.queue = q
	d.mu.Unlock()
}

// SetMetrics does late-binding injection of dispatcher metrics. Init-order of keeper:
// audit-writer (with tap) built before metrics-registry, so metrics
// passed after (pattern vault.SetMetrics / rbacHolder.SetMetrics).
// nil-receiver / nil-metrics — no-op.
func (d *Dispatcher) SetMetrics(m *DispatcherMetrics) {
	if d == nil {
		return
	}
	d.metrics = m
}

// InvalidateRules flushes rule cache — next Dispatch will reread
// snapshot from source. Called by CRUD handlers of Tiding/Herald (S4) after
// create/update/delete for immediate change application in this process.
func (d *Dispatcher) InvalidateRules() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.cacheInit = false
	d.cached = nil
	d.mu.Unlock()
}

// Dispatch matches event against enabled rules and enqueues DeliveryJob on
// each match. Best-effort: errors from rule source / enqueue
// logged but not propagated (tap shouldn't affect audit write-path).
//
// Non-run-scope event (any keeper-event outside run scopes) filtered
// cheaply before rule load: matchEventType won't fire, but area check
// saves extra rule traversal for CRUD/lifecycle noise.
func (d *Dispatcher) Dispatch(ctx context.Context, event *audit.Event) {
	if d == nil || event == nil {
		return
	}
	// Loop-guard (ADR-052(d)): own delivery terminals `herald.*` themselves
	// go through audit-writer → tap → here. Filter them before rule load —
	// "notification about notification" shouldn't become loop (guard over
	// CRUD validation, see isHeraldOwnEvent).
	if isHeraldOwnEvent(event.EventType) {
		return
	}
	rules, err := d.rules(ctx)
	if err != nil {
		d.logger.Warn("herald: dispatch skipped, rules load failed",
			slog.String("event_type", string(event.EventType)),
			slog.Any("error", err))
		d.metrics.observeError()
		return
	}

	// Snapshot queue under RLock — late-binding SetQueue (setupRedis) may substitute
	// её конкурентно с Dispatch (tap-consumer-горутина).
	d.mu.RLock()
	queue := d.queue
	d.mu.RUnlock()

	// occurred_at момента матча: event.CreatedAt чаще всего zero — инициаторы
	// write-path-а полагаются на PG `DEFAULT NOW()` (auditpg.Write пишет время в
	// строку БД, но НЕ обратно в *event), поэтому к моменту tap-наблюдения поле
	// нулевое. Берём время матча (d.clock — момент постановки job-а, ближайшее к
	// audit-INSERT-у наблюдаемое нам время), а CreatedAt используем только когда
	// инициатор проставил его явно (нечастый случай). Иначе occurred_at в webhook-
	// теле был бы 0001-01-01 (баг live-smoke Herald).
	occurredAt := occurredAt(event, d.clock())

	matched := 0
	for _, t := range rules {
		if !matchTiding(t, event) {
			continue
		}
		matched++
		job := &DeliveryJob{
			ID:            audit.NewULID(),
			Herald:        t.Herald,
			Tiding:        t.Name,
			EventType:     event.EventType,
			CorrelationID: event.CorrelationID,
			OccurredAt:    occurredAt,
			PayloadCopy:   copyPayload(event.Payload),
			// Annotations/Projection переносятся в job из Tiding, но dispatcher
			// их НЕ применяет (ADR-052(h): merge/projection — off-path в worker-е
			// при сборке webhookPayload, N3). Здесь только перенос — worker (N3)
			// читает эти поля; пока игнорирует (заглушка).
			Annotations: t.Annotations,
			Projection:  t.Projection,
		}
		if err := queue.Enqueue(ctx, job); err != nil {
			d.logger.Warn("herald: enqueue delivery job failed",
				slog.String("tiding", t.Name),
				slog.String("herald", t.Herald),
				slog.Any("error", err))
			d.metrics.observeError()
			continue
		}
	}
	d.metrics.observeDispatch(matched)
}

// rules возвращает снимок enabled-правил, перечитывая из source при холодном
// кэше или истёкшем TTL. Под RWMutex: быстрый путь (read-lock) на тёплом кэше.
func (d *Dispatcher) rules(ctx context.Context) ([]*Tiding, error) {
	now := d.clock()

	d.mu.RLock()
	if d.cacheInit && now.Sub(d.cachedAt) < d.ttl {
		rules := d.cached
		d.mu.RUnlock()
		return rules, nil
	}
	d.mu.RUnlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	// Повторная проверка под write-lock: другой вызов мог обновить кэш, пока
	// мы ждали lock (single-flight без отдельной группы — refresh дёшев).
	if d.cacheInit && now.Sub(d.cachedAt) < d.ttl {
		return d.cached, nil
	}
	rules, err := d.source.EnabledTidings(ctx)
	if err != nil {
		// Кэш не трогаем: при сбое source держим прежний снимок (если был),
		// но возвращаем ошибку — Dispatch её залогирует и пропустит событие.
		// Прежний снимок остаётся валидным для следующих событий до сходимости.
		return nil, err
	}
	d.cached = rules
	d.cachedAt = now
	d.cacheInit = true
	return rules, nil
}

// matchTiding — true, если событие проходит ВСЕ условия Tiding-правила
// (ADR-052(c)): хотя бы один event_type-паттерн покрывает тип события И
// (only_failures ⇒ событие-провал) И (only_changes ⇒ событие несёт changes)
// И селекторы incarnation/cadence/task (если заданы) совпадают. task-селектор
// (ADR-052 §l) матчит только incarnation.run_completed с искомым адресом в
// changed_tasks (см. matchTask).
//
// Disabled-правила сюда не доходят — source отдаёт только enabled
// (tidings_enabled_idx). Пустой EventTypes невозможен (CHECK + валидация).
func matchTiding(t *Tiding, event *audit.Event) bool {
	if t == nil {
		return false
	}
	if !matchAnyEventType(t.EventTypes, event.EventType) {
		return false
	}
	if t.OnlyFailures && !isFailureEvent(event.EventType) {
		return false
	}
	if t.OnlyChanges && !hasChanges(event.EventType, event.Payload) {
		return false
	}
	// Ephemeral-правило (ADR-052(g)) сужено до СВОЕГО прогона: VoyageID-селектор
	// матчит только события этого Voyage. Постоянные правила (VoyageID nil)
	// проходят как раньше — matchVoyage возвращает true.
	if !matchVoyage(t.VoyageID, event.CorrelationID, event.Payload) {
		return false
	}
	if !matchIncarnation(t.Incarnation, event.EventType, event.Payload) {
		return false
	}
	if !matchCadence(t.Cadence, event.EventType, event.Payload) {
		return false
	}
	if !matchTask(t.Task, event.EventType, event.Payload) {
		return false
	}
	return true
}

func matchAnyEventType(patterns []string, et audit.EventType) bool {
	for _, p := range patterns {
		if matchEventType(p, et) {
			return true
		}
	}
	return false
}

// occurredAt выбирает occurred_at для DeliveryJob: явный event.CreatedAt, если
// инициатор его проставил, иначе fallback на now (момент матча). Причина
// fallback — auditpg.Write оставляет event.CreatedAt нулевым при опоре на PG
// `DEFAULT NOW()`, а tap наблюдает тот же указатель уже после INSERT-а (см.
// вызов в Dispatch). Возвращаемое время — UTC.
func occurredAt(event *audit.Event, now time.Time) time.Time {
	if !event.CreatedAt.IsZero() {
		return event.CreatedAt.UTC()
	}
	return now.UTC()
}

// copyPayload — копия ТОЛЬКО верхнего уровня payload-map: новый map с теми же
// значениями по ссылке. Вложенные map/slice НЕ изолированы — глубокая мутация
// (например, payload["summary"].(map)["x"]=…) видна и через копию, и через
// оригинал. Это осознанный trade-off: здесь payload ещё НЕ замаскирован (сырой
// in-process *event, tap видит его до маскинга), а это лишь read-only снимок для
// доставки — маскинг секретов делается позже на доставке в worker.buildPayload
// (MaskSecrets). Глубокое копирование на горячем write-path не оправдано; копия
// защищает лишь от подмены/добавления ключей верхнего уровня общего указателя.
// nil → nil.
func copyPayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
