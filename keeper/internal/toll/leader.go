package toll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// sortedKeys возвращает отсортированные ключи map[string]float64 — общий
// helper для детерминированного per-coven перебора (без него map-итерация
// даёт нестабильный выбор coven при множественных одновременных trigger-ах).
func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SortedSetReader — узкая поверхность чтения disconnect-окна из общего
// sorted-set + чистки старых записей. Сужение даёт fake для unit-тестов
// Leader-а без живого Redis-а.
type SortedSetReader interface {
	// CountInWindow — ZCOUNT sorted-set по range [fromUnix, toUnix].
	CountInWindow(ctx context.Context, fromUnix, toUnix int64) (int64, error)
	// TrimBelow — ZREMRANGEBYSCORE [-inf, beforeUnix]. Idempotent.
	TrimBelow(ctx context.Context, beforeUnix int64) error
}

// CovenAwareReader — опц. расширение [SortedSetReader] для per-coven
// группировки (ADR-038 amendment, extensions). Реализуется только в
// production-адаптере keeperRedisTollSortedSetReader; в unit-fake-ах не
// обязателен (Leader получает nil-counters → per-coven trigger no-op).
//
// Возвращает count disconnect-ов в окне сгруппированно по coven-метке
// (extract по последнему `|`-сегменту member-value, ADR-038 schema
// `sid|kid|coven|nano`). Пустой coven попадает в ключ "" (тот же стиль,
// что у Prometheus counter-а disconnectsTotal{coven=""}).
type CovenAwareReader interface {
	// CountByCovenInWindow — ZRANGEBYSCORE [fromUnix, toUnix] → group-by coven.
	// Используется только при заданном per_coven_thresholds (иначе один
	// лишний round-trip за окно). Возвращает map[coven]count.
	CountByCovenInWindow(ctx context.Context, fromUnix, toUnix int64) (map[string]int64, error)
}

// LeaseAcquirer — узкая поверхность Redis-lease (Acquire/Renew/Release). В
// production обёрнут [keeperredis.Lease]; unit-тесты Leader-а подменяют на
// fake. Метод Acquire возвращает «лидер? либо причина» — ErrLeaseTaken
// должен быть распознан caller-ом (Leader делает sleep и ретраит).
type LeaseAcquirer interface {
	Acquire(ctx context.Context, key, holder string, ttl time.Duration) (Lease, error)
}

// Lease — held lease handle. Renew возвращает ErrLeaseLost при переходе
// lease-а к другому holder-у. Release всегда idempotent.
type Lease interface {
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
}

// ErrLeaseTaken / ErrLeaseLost — sentinel, переброшенные из keeperredis-пакета
// для общего API Leader-а (тесты сравнивают через errors.Is без импорта
// keeperredis).
var (
	ErrLeaseTaken = errors.New("toll: lease already taken")
	ErrLeaseLost  = errors.New("toll: lease lost (no longer leader)")
)

// Notifier — узкая поверхность alert-out-а на set/clear cluster:degraded
// (ADR-038 amendment, extensions). Реализация — [WebhookNotifier] в
// webhook.go; fake-ы в leader_test.go. Notify вызывается best-effort:
// ошибка логируется внутри реализации, наружу не возвращается (Leader
// не должен прерывать loop из-за webhook-flap-а; symmetric с audit.Writer
// failure-handling).
type Notifier interface {
	// Notify отправляет один alert-event. Реализация сама определяет
	// формат сериализации (generic / pagerduty_v2 / slack) — Leader
	// передаёт нормализованную TollEvent.
	Notify(ctx context.Context, event TollEvent)
}

// TollEvent — нормализованная форма event-а для Notifier. Подмножество
// audit-payload-а без операторских полей.
type TollEvent struct {
	// Type — "degraded_set" / "degraded_cleared".
	Type string
	// LeaderKID — KID инстанса, взведшего/снявшего флаг.
	LeaderKID string
	// Rate — disconnect_rate / baseline на момент events.
	Rate float64
	// BaselineConnected — `souls.status='connected'` snapshot.
	BaselineConnected int64
	// Threshold — порог, который пересёк rate (top-level или per-coven).
	Threshold float64
	// WindowSeconds — длина sliding-окна, в секундах.
	WindowSeconds int
	// CovenName — имя coven при per-coven trigger-е; пусто при global.
	CovenName string
	// Timestamp — момент срабатывания (UTC).
	Timestamp time.Time
}

// EventTypeDegradedSet / EventTypeDegradedCleared — closed-enum значения
// [TollEvent.Type]. Используются и реализациями Notifier-а, и тестами.
const (
	EventTypeDegradedSet     = "degraded_set"
	EventTypeDegradedCleared = "degraded_cleared"
)

// LeaderConfig — параметры Leader-loop-а.
type LeaderConfig struct {
	// KID — идентификатор инстанса (holder lease-а, leader_kid в audit-payload).
	KID string

	// LeaseTTL — TTL lease-ключа cluster:toll:leader. Renew каждые LeaseTTL/3.
	LeaseTTL time.Duration
	// AcquireRetry — пауза между попытками Acquire (когда другой инстанс
	// держит lease). <=0 → дефолт [defaultAcquireRetry] (5s).
	AcquireRetry time.Duration
	// TickInterval — период aggregation-тика leader-а (как часто читать
	// sorted-set + compute rate). <=0 → дефолт [defaultTickInterval] (5s).
	TickInterval time.Duration

	// WindowSize — sliding-окно, по которому считается rate (ADR-038(d)).
	WindowSize time.Duration
	// Threshold — порог rate-а от baseline (0.20 = 20%).
	Threshold float64
	// DegradedTTL — TTL Redis-ключа cluster:degraded.
	DegradedTTL time.Duration
	// ClearGrace — устойчивое окно низкого rate-а до clearing (asymmetric
	// hysteresis).
	ClearGrace time.Duration
	// BaselineCacheTTL — TTL кеша baseline-snapshot-а (refresh каждые ttl).
	BaselineCacheTTL time.Duration

	// PerCovenThresholds — опц. per-coven threshold overrides (ADR-038
	// amendment, extensions). Если непуст и [LeaderDeps.SortedSet] реализует
	// [CovenAwareReader], leader дополнительно считает per-coven rate-ы и
	// взводит cluster:degraded при превышении ЛЮБОГО per-coven threshold-а.
	// Trigger по per-coven сохраняет имя coven в audit-payload (поле
	// coven_name); global-trigger остаётся без coven_name.
	//
	// Семантика OR (global ИЛИ per-coven) сознательная: global-уровень
	// продолжает реагировать на широкий отток (multiple coven понемногу),
	// per-coven — на локальные инциденты (один DC уехал в split).
	PerCovenThresholds map[string]float64

	// Notifier — опц. webhook alert-канал (ADR-038 amendment, extensions).
	// nil → degraded set/clear идёт без alert-out-а (audit + gauge + metrics
	// как было). Best-effort: ошибка Notify логируется, но Set/Clear не
	// блокирует (cluster degraded — primary goal).
	Notifier Notifier
}

// LeaderDeps — wire-up Leader-loop-а.
type LeaderDeps struct {
	Lease          LeaseAcquirer
	SortedSet      SortedSetReader
	DegradedWriter degradedWriter
	Baseline       BaselineReader
	Audit          audit.Writer
	Metrics        *Metrics
	Logger         *slog.Logger
}

const (
	defaultAcquireRetry = 5 * time.Second
	defaultTickInterval = 5 * time.Second
)

// Leader — фоновая goroutine, проводящая aggregation-тик при удержании
// Redis-lease (single-leader-инвариант, ADR-038).
//
// Жизненный цикл [Leader.Run]:
//  1. Try Acquire `cluster:toll:leader`. Конфликт → sleep AcquireRetry, retry.
//  2. После acquire — параллельный renew-loop (Renew каждые LeaseTTL/3).
//  3. Aggregation-loop: каждые TickInterval — ZCOUNT в окне, baseline, rate,
//     set/clear `cluster:degraded` с asymmetric hysteresis.
//  4. На ErrLeaseLost (renew) → выход из aggregation-loop, попытка повторного
//     acquire (мог быть split-brain / leader-flap).
//  5. На ctx.Done() → release lease, выход.
type Leader struct {
	// cfgMu защищает hot-reload-able поля cfg (см. [Leader.UpdateConfig]).
	// Tick читает значения под RLock; UpdateConfig подменяет под Lock. Поля
	// KID/LeaseTTL/AcquireRetry/TickInterval/BaselineCacheTTL — startup-only
	// (sleep-таймеры renew-loop / leader-election уже сидят в захваченных
	// duration-копиях), на hot-reload они НЕ применяются: рестарт-required
	// поля по [ADR-021(e)], симметрично policy `logging.file`.
	cfgMu sync.RWMutex
	cfg   LeaderConfig
	deps  LeaderDeps

	// Состояние asymmetric hysteresis (только в leader-loop горутине, без
	// мьютекса; единственная писательница — sameg).
	degradedSet     bool
	belowSince      time.Time // момент, с которого rate ≤ threshold непрерывно
	lastClearedRate float64
	lastBaseline    int64
}

// NewLeader валидирует Config/Deps и собирает Leader. Caller (daemon)
// запускает [Leader.Run] в отдельной goroutine.
func NewLeader(cfg LeaderConfig, deps LeaderDeps) (*Leader, error) {
	if cfg.KID == "" {
		return nil, errors.New("toll.NewLeader: empty KID")
	}
	if cfg.LeaseTTL <= 0 {
		return nil, errors.New("toll.NewLeader: LeaseTTL must be > 0")
	}
	if cfg.WindowSize <= 0 {
		return nil, errors.New("toll.NewLeader: WindowSize must be > 0")
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		return nil, fmt.Errorf("toll.NewLeader: Threshold must be in (0, 1], got %v", cfg.Threshold)
	}
	if cfg.DegradedTTL <= 0 {
		return nil, errors.New("toll.NewLeader: DegradedTTL must be > 0")
	}
	if cfg.ClearGrace <= 0 {
		return nil, errors.New("toll.NewLeader: ClearGrace must be > 0")
	}
	if deps.Lease == nil {
		return nil, errors.New("toll.NewLeader: nil Lease")
	}
	if deps.SortedSet == nil {
		return nil, errors.New("toll.NewLeader: nil SortedSet")
	}
	if deps.DegradedWriter == nil {
		return nil, errors.New("toll.NewLeader: nil DegradedWriter")
	}
	if deps.Baseline == nil {
		return nil, errors.New("toll.NewLeader: nil Baseline")
	}
	if deps.Logger == nil {
		return nil, errors.New("toll.NewLeader: nil Logger")
	}
	if cfg.AcquireRetry <= 0 {
		cfg.AcquireRetry = defaultAcquireRetry
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	if cfg.BaselineCacheTTL <= 0 {
		cfg.BaselineCacheTTL = cfg.WindowSize
	}
	return &Leader{cfg: cfg, deps: deps}, nil
}

// UpdateConfig атомарно подменяет hot-reload-able поля Leader-конфига
// ([ADR-021](docs/architecture.md) hot-reload). Безопасен для конкурентного
// вызова с tick-loop-ом: tick читает snapshot своих полей под RLock, новый
// snapshot увидит уже обновлённые значения.
//
// Обновляются: Threshold, WindowSize, DegradedTTL, ClearGrace, PerCovenThresholds,
// Notifier. KID/LeaseTTL/AcquireRetry/TickInterval/BaselineCacheTTL —
// restart-required (захвачены в renew-loop / leader-election sleep-таймерах),
// конфиг-валидация на reload их сравнивает с текущими и игнорирует тихо.
//
// Notifier подменяется по pointer-у: caller (daemon) собирает новый
// [WebhookNotifier] при mutation `toll.webhook.*` (vault-resolve, URL и т.п.) и
// передаёт его сюда. Старый notifier GC-ится естественно (нет ресурсов,
// требующих явного Close: http.Client держит только idle-conn pool).
//
// Валидация полей повторяет [NewLeader]: невалидное значение возвращает error
// БЕЗ применения swap-а (старый снимок остаётся актуальным — параллель с
// [Store.Reload]-семантикой).
func (l *Leader) UpdateConfig(newCfg LeaderConfig) error {
	if newCfg.WindowSize <= 0 {
		return errors.New("toll.Leader.UpdateConfig: WindowSize must be > 0")
	}
	if newCfg.Threshold <= 0 || newCfg.Threshold > 1 {
		return fmt.Errorf("toll.Leader.UpdateConfig: Threshold must be in (0, 1], got %v", newCfg.Threshold)
	}
	if newCfg.DegradedTTL <= 0 {
		return errors.New("toll.Leader.UpdateConfig: DegradedTTL must be > 0")
	}
	if newCfg.ClearGrace <= 0 {
		return errors.New("toll.Leader.UpdateConfig: ClearGrace must be > 0")
	}

	l.cfgMu.Lock()
	defer l.cfgMu.Unlock()
	l.cfg.WindowSize = newCfg.WindowSize
	l.cfg.Threshold = newCfg.Threshold
	l.cfg.DegradedTTL = newCfg.DegradedTTL
	l.cfg.ClearGrace = newCfg.ClearGrace
	l.cfg.PerCovenThresholds = newCfg.PerCovenThresholds
	l.cfg.Notifier = newCfg.Notifier
	return nil
}

// CurrentNotifier возвращает текущий [Notifier] под RLock. Нужен caller-у
// (daemon-applyTollReload), который пропускает webhook-recycle при отсутствии
// diff-а и передаёт обратно тот же notifier в [Leader.UpdateConfig].
func (l *Leader) CurrentNotifier() Notifier {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	return l.cfg.Notifier
}

// Run — блокирующий leader-loop. Завершается по ctx.Done(). Caller (daemon)
// gate-ит cleanup-стеком LIFO: cancel ctx → join goroutine.
func (l *Leader) Run(ctx context.Context) {
	baseline := newCachedBaseline(l.deps.Baseline, l.cfg.BaselineCacheTTL)
	for {
		if ctx.Err() != nil {
			return
		}
		lease, err := l.deps.Lease.Acquire(ctx, LeaseKey, l.cfg.KID, l.cfg.LeaseTTL)
		if err != nil {
			if errors.Is(err, ErrLeaseTaken) {
				l.deps.Logger.Debug("toll: lease held by another keeper — sleeping",
					slog.Duration("retry_in", l.cfg.AcquireRetry))
			} else {
				l.deps.Logger.Warn("toll: lease acquire failed — sleeping",
					slog.Any("error", err),
					slog.Duration("retry_in", l.cfg.AcquireRetry))
			}
			if !sleepCtx(ctx, l.cfg.AcquireRetry) {
				return
			}
			continue
		}

		l.deps.Logger.Info("toll: leader-election won",
			slog.String("kid", l.cfg.KID),
			slog.Duration("lease_ttl", l.cfg.LeaseTTL))
		l.deps.Metrics.SetLeaderActive(true)

		l.runAsLeader(ctx, lease, baseline)
		l.deps.Metrics.SetLeaderActive(false)
		// На любом выходе из runAsLeader: сбрасываем gauge cluster_degraded
		// в 0 (этот инстанс больше не leader, нечего set-ить). Реальный флаг
		// в Redis либо стоит (TTL гасит) либо снят (наш ClearDegraded ниже).
		l.deps.Metrics.SetClusterDegraded(false)
	}
}

// runAsLeader — внутренний цикл при удержании lease. Завершается на:
//   - ctx.Done() (graceful shutdown) → Release + return;
//   - ErrLeaseLost от renew (split-brain) → Release + return (внешний Run
//     попробует re-acquire).
func (l *Leader) runAsLeader(ctx context.Context, lease Lease, baseline *cachedBaseline) {
	defer func() {
		// Detached release-ctx (как в keeperredis.eventstream): stream-ctx
		// мог уже отмениться (graceful shutdown), но lease отдать надо.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := lease.Release(relCtx); err != nil {
			l.deps.Logger.Warn("toll: lease release failed",
				slog.Any("error", err))
		}
	}()

	// Renew-goroutine: периодически Renew. На ErrLeaseLost сигналит main-loop
	// через cancel renewCtx → внутренний select выйдет.
	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	renewDone := make(chan error, 1)
	go func() {
		every := l.cfg.LeaseTTL / 3
		if every <= 0 {
			every = l.cfg.LeaseTTL
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-renewCtx.Done():
				renewDone <- nil
				return
			case <-t.C:
				if err := lease.Renew(renewCtx); err != nil {
					if errors.Is(err, ErrLeaseLost) {
						renewDone <- ErrLeaseLost
						return
					}
					l.deps.Logger.Warn("toll: lease renew failed",
						slog.Any("error", err))
				}
			}
		}
	}()

	tick := time.NewTicker(l.cfg.TickInterval)
	defer tick.Stop()

	// Первый тик сразу: оператор не должен ждать TickInterval-а после
	// leader-election-а (важно при failover).
	l.aggregationTick(ctx, baseline)

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-renewDone:
			if errors.Is(err, ErrLeaseLost) {
				l.deps.Logger.Warn("toll: lease lost — stepping down")
			}
			return
		case <-tick.C:
			l.aggregationTick(ctx, baseline)
		}
	}
}

// tickSnapshot — snapshot hot-reload-able cfg-полей на момент tick-а. Снимается
// один раз в начале [Leader.aggregationTick]; tick работает с локальными
// копиями, не блокируя [Leader.UpdateConfig] во время Redis/audit-вызовов.
type tickSnapshot struct {
	windowSize         time.Duration
	threshold          float64
	degradedTTL        time.Duration
	clearGrace         time.Duration
	perCovenThresholds map[string]float64
	notifier           Notifier
	kid                string
}

// snapshotCfg возвращает snapshot hot-reload-able полей под RLock. Map
// PerCovenThresholds shallow-copy НЕ делается: UpdateConfig подменяет ссылку
// атомарно (старый Leader-тик видит старую map, новый — новую), мутаций map
// в-месте не происходит.
func (l *Leader) snapshotCfg() tickSnapshot {
	l.cfgMu.RLock()
	defer l.cfgMu.RUnlock()
	return tickSnapshot{
		windowSize:         l.cfg.WindowSize,
		threshold:          l.cfg.Threshold,
		degradedTTL:        l.cfg.DegradedTTL,
		clearGrace:         l.cfg.ClearGrace,
		perCovenThresholds: l.cfg.PerCovenThresholds,
		notifier:           l.cfg.Notifier,
		kid:                l.cfg.KID,
	}
}

// aggregationTick — один проход aggregation-loop-а: ZCOUNT за окно, baseline,
// rate, set/clear cluster:degraded.
func (l *Leader) aggregationTick(ctx context.Context, baseline *cachedBaseline) {
	snap := l.snapshotCfg()
	now := time.Now()
	from := now.Add(-snap.windowSize).Unix()
	to := now.Unix()

	count, err := l.deps.SortedSet.CountInWindow(ctx, from, to)
	if err != nil {
		l.deps.Logger.Warn("toll: ZCOUNT failed — skipping tick",
			slog.Any("error", err))
		return
	}

	base, baseErr := baseline.get(ctx, now)
	if baseErr != nil {
		if base == 0 {
			// Нет stale-значения — пропускаем тик (false-positive хуже, чем
			// одна потеря тика).
			l.deps.Logger.Warn("toll: baseline fetch failed and no stale — skipping tick",
				slog.Any("error", baseErr))
			return
		}
		// Stale-fallback: продолжаем, но логируем.
		l.deps.Logger.Warn("toll: baseline fetch failed — using stale",
			slog.Any("error", baseErr),
			slog.Int64("stale_baseline", base))
	}

	// Trim — лучше делать ПОСЛЕ ZCOUNT (если ZCOUNT упал на флапе, окно ещё
	// нетронуто, следующий тик повторит). Идемпотентно.
	if err := l.deps.SortedSet.TrimBelow(ctx, from); err != nil {
		l.deps.Logger.Debug("toll: ZREMRANGEBYSCORE failed (non-fatal)",
			slog.Any("error", err))
	}

	// Защита от деления на ноль: baseline=0 → не оцениваем (свежий кластер
	// без зарегистрированных Souls, либо все в pending). ratio=0 трактуем
	// как «не превышен» (никаких degraded-флагов).
	var rate float64
	if base > 0 {
		rate = float64(count) / float64(base)
	}
	l.lastClearedRate = rate
	l.lastBaseline = base

	// Per-coven trigger проверяется при наличии конфигурации И при способности
	// reader-а группировать по coven. Если global уже превысил threshold —
	// per-coven анализ необязателен (cluster:degraded и так будет set), но мы
	// его всё равно делаем для уточнения coven_name в audit/webhook payload
	// (per-coven trigger даёт более точную диагностику оператору).
	triggeredCoven, triggeredThr := l.maybePerCovenTrigger(ctx, snap.perCovenThresholds, from, to, base)

	exceededGlobal := rate > snap.threshold
	exceededAny := exceededGlobal || triggeredCoven != ""

	switch {
	case exceededAny:
		// Превышен порог — set/refresh degraded. belowSince ресетим (grace-окно
		// перестало накапливаться).
		l.belowSince = time.Time{}
		if err := l.deps.DegradedWriter.SetDegraded(ctx, snap.kid, snap.degradedTTL); err != nil {
			l.deps.Logger.Warn("toll: SET cluster:degraded failed",
				slog.Any("error", err))
			return
		}
		l.deps.Metrics.SetClusterDegraded(true)
		if !l.degradedSet {
			l.degradedSet = true
			// Выбираем «причинный» порог и coven для diag/audit/webhook:
			// global-trigger перевешивает per-coven, чтобы при двойном
			// превышении в логах фигурировал global-rate. Per-coven trigger
			// выводится только если global не сработал (локальный инцидент).
			triggerThreshold := snap.threshold
			triggerCoven := ""
			if !exceededGlobal {
				triggerThreshold = triggeredThr
				triggerCoven = triggeredCoven
			}
			l.deps.Logger.Error("toll: cluster degraded — write-API blocked",
				slog.Float64("rate", rate),
				slog.Float64("threshold", triggerThreshold),
				slog.String("coven", triggerCoven),
				slog.Int64("disconnects", count),
				slog.Int64("baseline_connected", base),
				slog.Duration("window", snap.windowSize))
			auditDegradedSet(ctx, l.deps.Audit, l.deps.Logger,
				snap.kid, rate, base, triggerThreshold, int(snap.windowSize.Seconds()), triggerCoven)
			l.notifyWith(ctx, snap.notifier, TollEvent{
				Type:              EventTypeDegradedSet,
				LeaderKID:         snap.kid,
				Rate:              rate,
				BaselineConnected: base,
				Threshold:         triggerThreshold,
				WindowSeconds:     int(snap.windowSize.Seconds()),
				CovenName:         triggerCoven,
				Timestamp:         now.UTC(),
			})
		}
	default:
		// rate ≤ threshold: если degraded был set, копим grace-окно.
		if !l.degradedSet {
			// Нечего снимать; belowSince держим неактивным.
			return
		}
		if l.belowSince.IsZero() {
			l.belowSince = now
			l.deps.Logger.Info("toll: rate dropped below threshold — grace window started",
				slog.Float64("rate", rate),
				slog.Float64("threshold", snap.threshold),
				slog.Duration("grace", snap.clearGrace))
			return
		}
		if now.Sub(l.belowSince) < snap.clearGrace {
			// Grace ещё не истёк — не снимаем (но degraded TTL в Redis всё
			// равно гаснет естественно через DegradedTTL — это by design
			// fail-safe, leader не обязан удерживать его явно).
			return
		}
		// Grace выдержан — clearing.
		if err := l.deps.DegradedWriter.ClearDegraded(ctx); err != nil {
			l.deps.Logger.Warn("toll: DEL cluster:degraded failed",
				slog.Any("error", err))
			return
		}
		l.deps.Metrics.SetClusterDegraded(false)
		l.degradedSet = false
		l.belowSince = time.Time{}
		l.deps.Logger.Info("toll: cluster degraded cleared after grace",
			slog.Float64("rate", rate),
			slog.Int64("baseline_connected", base),
			slog.Duration("grace", snap.clearGrace))
		auditDegradedCleared(ctx, l.deps.Audit, l.deps.Logger,
			snap.kid, rate, base, int(snap.clearGrace.Seconds()))
		l.notifyWith(ctx, snap.notifier, TollEvent{
			Type:              EventTypeDegradedCleared,
			LeaderKID:         snap.kid,
			Rate:              rate,
			BaselineConnected: base,
			Threshold:         snap.threshold,
			WindowSeconds:     int(snap.windowSize.Seconds()),
			Timestamp:         now.UTC(),
		})
	}
}

// maybePerCovenTrigger — опц. per-coven анализ (ADR-038 amendment, extensions).
// Возвращает (coven, threshold) первой найденной coven, чьё rate превысило
// конфигурированный per-coven threshold; ("", 0) — никакой trigger / per-coven
// не сконфигурирован / SortedSet не поддерживает группировку.
//
// Никаких ошибок наружу: per-coven trigger — дополнение, не должен ломать
// global-loop при Redis-флапе на ZRANGEBYSCORE.
//
// perCovenThresholds передаётся через tick-snapshot (см. [tickSnapshot]): чтение
// прямого `l.cfg.PerCovenThresholds` тут было бы race с [Leader.UpdateConfig].
func (l *Leader) maybePerCovenTrigger(ctx context.Context, perCovenThresholds map[string]float64, from, to int64, base int64) (string, float64) {
	if len(perCovenThresholds) == 0 || base <= 0 {
		return "", 0
	}
	reader, ok := l.deps.SortedSet.(CovenAwareReader)
	if !ok {
		return "", 0
	}
	counts, err := reader.CountByCovenInWindow(ctx, from, to)
	if err != nil {
		l.deps.Logger.Debug("toll: per-coven CountByCoven failed (non-fatal)",
			slog.Any("error", err))
		return "", 0
	}
	// Детерминированный порядок при множественных triggered-coven (multiple
	// одновременно превысили): берём в порядке config-ключей-сортированно.
	// Без сортировки — выбор зависит от map-итерации, что нестабильно для
	// тестов и логов. Перебираем threshold-ы (узкий набор), сверяемся со
	// counts.
	for _, coven := range sortedKeys(perCovenThresholds) {
		thr := perCovenThresholds[coven]
		c, present := counts[coven]
		if !present {
			continue
		}
		rate := float64(c) / float64(base)
		if rate > thr {
			return coven, thr
		}
	}
	return "", 0
}

// notifyWith — best-effort wrapper: nil-safe + recover на panic из реализации
// (Webhook-side bug не должен ронять leader-loop). notifier передаётся через
// tick-snapshot, чтобы избежать race с [Leader.UpdateConfig].
func (l *Leader) notifyWith(ctx context.Context, n Notifier, ev TollEvent) {
	if n == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			l.deps.Logger.Warn("toll: notifier panic recovered",
				slog.Any("panic", r),
				slog.String("event_type", ev.Type))
		}
	}()
	n.Notify(ctx, ev)
}

// sleepCtx спит указанное время, прерывается по ctx.Done(). Возвращает true,
// если sleep дошёл до конца; false — если ctx отменён.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
