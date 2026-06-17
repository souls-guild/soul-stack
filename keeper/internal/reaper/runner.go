// Runner — фоновый loop Жнеца (см. docs/keeper/reaper.md, ADR-006(d)).
//
// Лидерство (Redis-lease acquire / renew `lock_ttl/3` / re-acquire при потере /
// graceful Release) вынесено в generic [leaderloop.Loop] — общий каркас
// HA-singleton-задач Keeper-кластера. Runner — тонкий потребитель: задаёт
// leaseKey, tick-callback ([Runner.dispatch]) и hot-reload-функции интервала и
// lock_ttl поверх своего cfg-кэша. lease-семантика после выноса не изменилась.
//
// Зона ответственности Runner-а:
//
//   - dispatch правил: tick → читаем свежий cfg → исполняем включённые правила.
//     `time.Ticker`-интервал = `reaper.interval` (cron-grammar не нужен).
//   - Hot-reload — Runner подписывается на Store через [config.Store.OnReload] и
//     кэширует свежий snapshot в `atomic.Pointer[KeeperConfig]`. tick/intervalFn/
//     lockTTLFn читают atomic-pointer без обращения к Store; subscriber обновляет
//     pointer **немедленно** на swap-е. `enabled`/`interval`/`lock_ttl`/`max_age`/
//     `batch_size`/`dry_run` обновляются без рестарта.
//   - lease-Gauge `keeper_reaper_lease_held` — через OnLeaseChange-callback
//     leaderloop-а (1 на захвате, 0 на выходе из tick-loop-а).
//
// Restart-policy на потерю lease — внутри leaderloop (re-acquire); caller
// (runDaemon) просто крутит [Runner.Run] до SIGTERM.
package reaper

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/leaderloop"
	"github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

// leaseKey — Redis-ключ для лидерства Reaper-а. Зафиксирован в
// docs/keeper/reaper.md и не выносится в конфиг — это инвариант кластера
// (одно правило = один ключ, изменение требует ADR-update).
const leaseKey = "reaper:leader"

// Дефолты на случай пустых полей в keeper.yml (parser оставляет zero-value).
// Совпадают с docs/keeper/reaper.md (Конфиг).
const (
	defaultInterval       = time.Hour
	defaultLockTTL        = 5 * time.Minute
	defaultRuleBatch      = 1000
	defaultPurgeMaxAge    = 365 * 24 * time.Hour
	defaultAcquireBackoff = 5 * time.Second

	// Per-rule defaults max_age / stale_after — docs/keeper/reaper.md.
	// Используются, если cfg-значение пустое (semantic-validate уже
	// отверг бы невалидный формат; пустое — норма для опускания поля).
	defaultExpirePendingSeedsMaxAge = 24 * time.Hour
	defaultPurgeUsedTokensMaxAge    = 90 * 24 * time.Hour
	defaultPurgeSoulsMaxAge         = 30 * 24 * time.Hour
	defaultPurgeOldSeedsMaxAge      = 90 * 24 * time.Hour
	defaultMarkDisconnectedStale    = 90 * time.Second
	defaultPurgeApplyRunsMaxAge     = 30 * 24 * time.Hour

	// Retention истории Voyage-прогонов (`purge_voyages`, ADR-046 §79). 30d
	// ВЫРОВНЕНО на defaultPurgeApplyRunsMaxAge сознательно: voyage_targets
	// несут soft-link apply_id на apply_runs (без FK), и drill «voyage → его
	// apply_runs» должен видеть обе стороны до одного момента — иначе voyage
	// удалён, а apply_runs ещё живут (или наоборот), и All-runs вид теряет
	// корреляцию. Менять одно окно без другого — рассинхрон drill-а.
	defaultPurgeVoyagesMaxAge = 30 * 24 * time.Hour

	// Retention растущей run-history push-прогонов (`purge_push_runs`,
	// миграция 076). 30d — parity с defaultPurgeApplyRunsMaxAge: push_runs —
	// run-history таблица того же класса, что apply_runs/voyages, и держать
	// её хвост по тому же окну избавляет от рассинхрона при дрилле «push-run
	// → его per-host summary». НЕ путать с TTL правила
	// `purge_orphan_push_runs` (1h, терминализация зомби) — разные правила,
	// разные окна.
	defaultPurgePushRunsMaxAge = 30 * 24 * time.Hour

	// Retention архивных данных compliance-класса (`purge_incarnation_archive` /
	// `purge_state_history_archive` / `purge_archived_state_history`, миграция
	// 077). 365d — СОЗНАТЕЛЬНО консервативнее run-history-окон (30d): архив
	// (incarnation_archive / state_history_archive из 039 + soft-deleted
	// state_history из 048) — это историко-compliance данные снесённых
	// incarnation, которые оператор может удержать год по требованиям аудита.
	// Настраивается через keeper.yml → reaper.rules.<rule>.max_age; в примере
	// стоит 365d с пометкой «compliance-окно». Возраст — от archived_at.
	defaultPurgeArchiveMaxAge = 365 * 24 * time.Hour

	// Grace после терминала apply_run, по истечении которого register-строки
	// прогона удаляются. Короткий (1h, не 30d как у самого apply_run): register
	// нужен только до барьера, после терминала это plaintext-мусор. Заведомо
	// больше времени «барьер → чтение register» при cross-Keeper-роутинге.
	defaultPurgeApplyTaskRegisterGrace = time.Hour

	// Формальный duration-аргумент правила reclaim_apply_runs: recovery
	// сравнивает claim_expires_at < NOW() напрямую (lease уже зашит в
	// claim_expires_at при захвате Ward-а), поэтому это значение в предикат
	// не входит. Держим осмысленный дефолт для единообразия duration-runner-а.
	defaultReclaimApplyRunsLease = time.Minute

	// Grace по возрасту Vault-секрета для reap_orphan_vault_keys: секрет
	// `secret/keeper/sigil-keys/<key_id>` считается осиротевшим только если он
	// старше этого порога. Отсекает гонку с Introduce (write-в-Vault до
	// PG-commit-а): свежий секрет ещё может получить строку в sigil_signing_keys.
	// 24h — щедрый запас сверх любого реалистичного окна Introduce.
	defaultReapOrphanVaultKeysGrace = 24 * time.Hour

	// Дефолты правила archive_state_history (ADR-Q19 retention,
	// docs/keeper/reaper.md). N=50 — продукт-решение пользователя: достаточно,
	// чтобы Operator API всегда мог показать «последний месяц активности»
	// типового incarnation (1-2 apply в день), и достаточно мало, чтобы хвост
	// state_history не рос без архива. keep_version_bump=true — защита снимков
	// шагов state_schema-миграции (scenario='migration') от soft-delete;
	// restorable anchor для recovery схемы при rollback ADR-019.
	defaultArchiveStateHistoryKeepLastN       = 50
	defaultArchiveStateHistoryKeepVersionBump = true
)

// Default statuses для правил со statuses[]-фильтром (docs/keeper/reaper.md).
// Используются, если cfg.statuses пустой — например, оператор включил
// правило одной строкой `enabled: true` без переопределения списка.
var (
	defaultPurgeSoulsStatuses    = []string{"disconnected", "expired"}
	defaultPurgeOldSeedsStatuses = []string{"superseded", "expired", "revoked"}
)

// PurgerAPI — узкий интерфейс, который дёргает Runner. Сужение для
// unit-тестов: подставляем fake без поднятия PG. Реальный [*Purger]
// удовлетворяет автоматически.
type PurgerAPI interface {
	PurgeAuditOld(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeExpiredPendingTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeUsedTokens(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeSouls(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error)
	PurgeOldSeeds(ctx context.Context, statuses []string, maxAge time.Duration, batchSize int) (int64, error)
	MarkDisconnected(ctx context.Context, staleAfter time.Duration, batchSize int) (int64, error)
	PurgeApplyRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeVoyages(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgePushRuns(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeIncarnationArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeStateHistoryArchive(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeArchivedStateHistory(ctx context.Context, maxAge time.Duration, batchSize int) (int64, error)
	PurgeApplyTaskRegister(ctx context.Context, gracePeriod time.Duration, batchSize int) (int64, error)
	ReclaimApplyRuns(ctx context.Context, lease time.Duration, batchSize int) (int64, error)
	ReportOrphanVaultKeys(ctx context.Context, grace time.Duration, batchSize int) (int64, error)
	ArchiveStateHistory(ctx context.Context, keepLastN int, keepVersionBump bool, batchSize int) (int64, error)
}

// defaultPurgeOrphanPushRunsMaxAge — TTL правила `purge_orphan_push_runs`
// (docs/keeper/reaper.md). Push-прогон, висящий в pending/running дольше часа,
// почти наверняка осиротел (Keeper-инстанс умер во время выполнения, executeAsync
// не дописал терминал). 1h — заведомо больше реалистичного push-прогона
// (render+per-host SendApply за <30m в pilot-объёме); meaningful запас.
const defaultPurgeOrphanPushRunsMaxAge = time.Hour

// defaultPurgeOldErrandsMaxAge — формальный duration-аргумент правила
// `purge_old_errands` (ADR-033, docs/keeper/reaper.md). TTL хранится в самой
// строке `errands.ttl_at` (зашит при INSERT-е dispatcher-ом, по умолчанию
// `started_at + 7d`), поэтому предикат правила — `ttl_at < NOW()` без
// дополнительного offset-а. Это значение в SQL НЕ передаётся, но runner
// требует positive duration для общего пути parseRuleDuration. 7д держим
// для единообразия с errand.TTLDefault.
const defaultPurgeOldErrandsMaxAge = 7 * 24 * time.Hour

// defaultReclaimVoyagesLease — формальный duration-аргумент правила
// `reclaim_voyages` (ADR-043 S4). Recovery сравнивает `claim_expires_at < NOW()`
// напрямую (lease зашит в claim_expires_at при захвате через voyage.ClaimNext),
// значение в SQL-предикат НЕ входит. Держим осмысленный дефолт для единообразия
// duration-runner-а.
const defaultReclaimVoyagesLease = time.Minute

// defaultPurgeOrphanEphemeralTidingsGrace — grace правила
// `purge_orphan_ephemeral_tidings` (ADR-052(g) amendment N2). `max_age`
// семантически = grace ПОСЛЕ терминала Voyage и ВХОДИТ в предикат (как
// `purge_apply_task_register`). Grace — обязательное условие корректности:
// dispatcher матчит терминальное событие против ephemeral-правила асинхронно
// (tap-consumer-горутина через bounded-канал, ADR-052(c)); снос правила раньше,
// чем consumer дочитает событие и заэнкьюит уведомление, потерял бы уведомление
// о завершении. 5m — заведомо больше окна tap-consumer-а (bounded-канал
// разгребается за миллисекунды даже при шторме финализаций) с большим запасом
// на drain/retry; короче 7д errand-TTL, т.к. ephemeral-Tiding после доставки
// — мусор, держать его незачем.
const defaultPurgeOrphanEphemeralTidingsGrace = 5 * time.Minute

// Deps — внешние зависимости Runner-а. Все поля обязательны, кроме
// AcquireBackoff (по умолчанию — [defaultAcquireBackoff]).
type Deps struct {
	// Purger — исполнитель SQL-rule-ов. В Reaper.a — единственное
	// правило `purge_audit_old`.
	Purger PurgerAPI

	// Redis — клиент, через который захватывается lease лидерства.
	Redis *redis.Client

	// Store — снимок KeeperConfig с hot-reload-семантикой (M0.3).
	// Runner читает `cfg.Reaper.*` на каждой итерации.
	Store *config.Store[config.KeeperConfig]

	// Holder — идентификатор Keeper-инстанса (KID), записывается в lease-ключ.
	Holder string

	// Logger — slog-логгер. Структурированные поля: `key`, `holder`, `rule`,
	// `deleted`, `error`. Метрики через OTel — отдельный slice (см. reaper.md).
	Logger *slog.Logger

	// AcquireBackoff — пауза между попытками Acquire-а при конфликте
	// лидерства. На production-значениях ~5s достаточно крупно, чтобы не
	// флудить Redis, и достаточно мелкое, чтобы failover при падении
	// лидера происходил в пределах нескольких секунд. Поле в Deps (а не
	// package-level var), чтобы тесты могли подменять short-значение без
	// races под `go test -parallel`.
	//
	// Zero-value → [defaultAcquireBackoff].
	AcquireBackoff time.Duration

	// Metrics — Prometheus-collectors для per-rule метрик (executions /
	// purged / duration / errors) и lease-Gauge. Nil допустим — методы
	// [*ReaperMetrics] no-op-ят на nil-получателе (для unit-тестов
	// Runner-а без obs-стека).
	Metrics *ReaperMetrics

	// Scry — зависимости фонового drift-правила `scry_background`
	// (ADR-031 Slice C). Опционально: nil → правило в dispatch-е молча
	// пропускается с warn-ом (см. runScryBackground). Production wire-up
	// собирает [ScryDeps] в daemon.setupReaper.
	Scry *ScryDeps

	// OrphanPushRuns — зависимость правила `purge_orphan_push_runs`
	// (Variant C push orchestrator, docs/keeper/push.md). Опционально: nil →
	// правило в dispatch-е молча пропускается с warn-ом (паттерн Scry).
	// Production wire-up передаёт [*orphanPurger] из
	// [NewOrphanPushRunsPurger] поверх pushorch.Store.
	OrphanPushRuns *orphanPurger

	// OldErrands — зависимость правила `purge_old_errands` (ADR-033,
	// docs/keeper/reaper.md). Опционально: nil → правило в dispatch-е молча
	// пропускается с warn-ом (паттерн OrphanPushRuns). Production wire-up
	// передаёт [*ErrandsPurger] из [NewErrandsPurger] поверх d.pool.
	OldErrands *ErrandsPurger

	// VoyageReclaim — зависимость правила `reclaim_voyages` (ADR-043 S4,
	// docs/keeper/reaper.md). Опционально: nil → правило в dispatch-е молча
	// пропускается с warn-ом (паттерн OldErrands). Production wire-up передаёт
	// [*VoyageReclaimer] из [NewVoyageReclaimer] поверх d.pool.
	VoyageReclaim *VoyageReclaimer

	// OrphanEphemeralTidings — зависимость правила
	// `purge_orphan_ephemeral_tidings` (ADR-052(g) amendment N2,
	// docs/keeper/reaper.md). Сносит осиротевшие ephemeral-Tiding-и (прогон в
	// терминале > grace либо не существует). Опционально: nil → правило в
	// dispatch-е молча пропускается с warn-ом (паттерн VoyageReclaim).
	// Production wire-up передаёт [*EphemeralTidingsPurger] из
	// [NewEphemeralTidingsPurger] поверх d.pool.
	OrphanEphemeralTidings *EphemeralTidingsPurger
}

// Runner — корневая структура. Один экземпляр на keeper-процесс.
type Runner struct {
	deps           Deps
	acquireBackoff time.Duration

	// currentCfg — атомарный кэш последнего успешного снимка из Store.
	// Заполняется при NewRunner и обновляется callback-ом, подписанным
	// через [config.Store.OnReload] (см. ADR-021 + Architecture E выше).
	//
	// tick-loop читает только этот pointer и НЕ обращается к Store на
	// каждом тике: на successful reload subscriber обновляет cache
	// **немедленно**, и следующий tick (или текущая итерация acquire-loop)
	// уже видит новый cfg без латентности следующего tick-а.
	currentCfg atomic.Pointer[config.KeeperConfig]
}

// NewRunner проверяет deps и возвращает Runner. На отсутствующие
// обязательные зависимости — error: программная ошибка caller-а
// (runDaemon), не runtime-условие.
func NewRunner(d Deps) (*Runner, error) {
	if d.Purger == nil {
		return nil, errors.New("reaper.NewRunner: Purger is required")
	}
	if d.Redis == nil {
		return nil, errors.New("reaper.NewRunner: Redis is required")
	}
	if d.Store == nil {
		return nil, errors.New("reaper.NewRunner: Store is required")
	}
	if d.Holder == "" {
		return nil, errors.New("reaper.NewRunner: Holder is required (use cfg.KID)")
	}
	if d.Logger == nil {
		return nil, errors.New("reaper.NewRunner: Logger is required")
	}
	backoff := d.AcquireBackoff
	if backoff <= 0 {
		backoff = defaultAcquireBackoff
	}
	r := &Runner{deps: d, acquireBackoff: backoff}
	// Заполняем cache начальным snapshot-ом. Может быть nil — это валидное
	// состояние «initial load валился на validation», recovery — через
	// первый успешный Reload + subscriber. tick-loop защищён от nil-cfg.
	r.currentCfg.Store(d.Store.Get())
	// Subscriber обновляет cache на каждом успешном Reload-swap-е.
	// Unsubscribe не сохраняется: Runner живёт всё время жизни процесса,
	// освобождать подписку не требуется (Store умирает вместе с Runner-ом).
	d.Store.OnReload(func(_, newCfg *config.KeeperConfig) {
		r.currentCfg.Store(newCfg)
	})
	return r, nil
}

// Run крутит leader-loop до тех пор, пока ctx не отменится. Возвращает nil на
// graceful-stop (ctx.Done) и обёрнутую error на fatal-условия acquire-фазы.
//
// Лидерство, renewal, re-acquire и graceful-shutdown делегированы generic
// [leaderloop.Loop] (вынесенному из исходного Reaper-runner-а — lease-семантика
// идентична: lock_ttl/3 renew, backoff, immediate-tick, Release-on-stop).
// Reaper остаётся тонким потребителем: его tick-callback — это [Runner.dispatch]
// над свежим cfg-снимком, lease-Gauge — через OnLeaseChange, hot-reload
// interval/lock_ttl — через intervalFn/lockTTLFn поверх атомарного cfg-кэша.
func (r *Runner) Run(ctx context.Context) error {
	loop, err := leaderloop.New(leaderloop.Config{
		LeaseKey:       leaseKey,
		Holder:         r.deps.Holder,
		Redis:          r.deps.Redis,
		Logger:         r.deps.Logger,
		AcquireBackoff: r.acquireBackoff,
		IntervalFn:     r.tickInterval,
		LockTTLFn:      r.lockTTL,
		Tick:           r.tick,
		OnLeaseChange:  r.deps.Metrics.SetLeaseHeld,
	})
	if err != nil {
		// Все обязательные поля проверены NewRunner-ом → New здесь не должен
		// падать. Прокидываем на случай рассинхрона контрактов.
		return err
	}
	return loop.Run(ctx)
}

// tick — tick-callback для leaderloop: читает свежий cfg-снимок и диспетчеризует
// правила. Идентично исходному tickLoop-телу: nil-cfg (невалидный initial load)
// пропускается с warn-ом, не роняя loop.
func (r *Runner) tick(ctx context.Context) {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		r.deps.Logger.Warn("reaper: config snapshot is nil, skipping tick")
		return
	}
	r.dispatch(ctx, cfg)
}

// tickInterval — intervalFn для leaderloop: интервал между тиками из свежего
// cfg-снимка (hot-reload). nil-cfg → [defaultInterval], чтобы leaderloop всегда
// получал валидный positive-интервал (tick на nil-cfg сам пропустит работу).
func (r *Runner) tickInterval() time.Duration {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		return defaultInterval
	}
	return parseDurationOr(cfg.Reaper, defaultInterval, reaperInterval)
}

// lockTTL — lockTTLFn для leaderloop: TTL Redis-lease из свежего cfg-снимка
// (hot-reload между re-acquire). nil-cfg → [defaultLockTTL].
func (r *Runner) lockTTL() time.Duration {
	cfg := r.currentCfg.Load()
	if cfg == nil {
		return defaultLockTTL
	}
	return parseDurationOr(cfg.Reaper, defaultLockTTL, reaperLockTTL)
}

// dispatch применяет каждое включённое правило одно за другим. Знает
// все правила из docs/keeper/reaper.md; неизвестное имя — warn, чтобы
// опечатка в keeper.yml не висела молчаливым no-op-ом. Исключение —
// reclaim_voyages: default-ON через path-defaulting (ADR-043 §8),
// исполняется отдельной веткой dispatchReclaimVoyages после цикла.
func (r *Runner) dispatch(ctx context.Context, cfg *config.KeeperConfig) {
	if cfg.Reaper == nil || !cfg.Reaper.Enabled {
		return
	}
	dryRun := cfg.Reaper.DryRun
	batchSize := cfg.Reaper.BatchSize
	if batchSize <= 0 {
		batchSize = defaultRuleBatch
	}

	for name, rule := range cfg.Reaper.Rules {
		// reclaim_voyages — default-ON через path-defaulting в отдельной ветке ниже
		// (dispatchReclaimVoyages); из основного цикла исключён, чтобы при
		// `Enabled:true` правило не исполнилось дважды.
		if name == "reclaim_voyages" {
			continue
		}
		if !rule.Enabled {
			continue
		}
		switch name {
		case "purge_audit_old":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeAuditOld)
		case "expire_pending_seeds":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultExpirePendingSeedsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeExpiredPendingTokens)
		case "purge_used_tokens":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeUsedTokensMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeUsedTokens)
		case "purge_souls":
			r.runStatusesRule(ctx, name, rule.Statuses, defaultPurgeSoulsStatuses,
				rule.MaxAge, defaultPurgeSoulsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeSouls)
		case "purge_old_seeds":
			r.runStatusesRule(ctx, name, rule.Statuses, defaultPurgeOldSeedsStatuses,
				rule.MaxAge, defaultPurgeOldSeedsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeOldSeeds)
		case "mark_disconnected":
			r.runDurationRule(ctx, name, rule.StaleAfter, defaultMarkDisconnectedStale, batchSize, dryRun,
				r.deps.Purger.MarkDisconnected)
		case "purge_apply_runs":
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeApplyRunsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeApplyRuns)
		case "purge_voyages":
			// Retention растущей истории Voyage-прогонов (ADR-046 §79,
			// docs/keeper/reaper.md): finished voyages (succeeded/failed/
			// partial_failed/cancelled) старше `max_age` (default 30d) →
			// DELETE; voyage_targets уносятся ON DELETE CASCADE. scheduled/
			// pending/running НЕ трогаются. Окно по умолчанию выровнено на
			// purge_apply_runs — drill «voyage → apply_runs» обязан видеть обе
			// стороны до одного момента (см. defaultPurgeVoyagesMaxAge).
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeVoyagesMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeVoyages)
		case "purge_push_runs":
			// Retention растущей run-history push-прогонов (миграция 076,
			// docs/keeper/reaper.md): finished push_runs (success/
			// partial_failed/failed/cancelled) старше `max_age` (default 30d) →
			// DELETE. pending/running НЕ трогаются — это правило
			// `purge_orphan_push_runs` (терминализация зомби). Каскада нет —
			// per-host результаты inline в push_runs.summary (jsonb), дочерних FK
			// на push_runs нет (051). Окно по умолчанию выровнено на
			// purge_apply_runs (см. defaultPurgePushRunsMaxAge).
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgePushRunsMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgePushRuns)
		case "purge_incarnation_archive":
			// Retention архива снесённых incarnation (incarnation_archive,
			// миграция 039; SQL 077): строки с archived_at старше `max_age`
			// (default 365d — compliance-окно, КОНСЕРВАТИВНЕЕ run-history 30d) →
			// DELETE. Дочерних FK на архив нет (039), каскада нет. Возраст — от
			// archived_at.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeIncarnationArchive)
		case "purge_state_history_archive":
			// Retention архива журнала state_history снесённых incarnation
			// (state_history_archive, миграция 039; SQL 077): строки с
			// archived_at старше `max_age` (default 365d) → DELETE. Parity
			// purge_incarnation_archive; дочерних FK нет.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeStateHistoryArchive)
		case "purge_archived_state_history":
			// Физический снос soft-deleted-снимков (archived_at IS NOT NULL) из
			// ЖИВОЙ state_history (миграция 048; SQL 077) старше `max_age`
			// (default 365d). НЕ путать с archive_state_history (049), который
			// ТОЛЬКО проставляет soft-delete-флаг — это правило сносит уже
			// помеченные строки по истечении compliance-окна. Активные снимки
			// (archived_at IS NULL) НЕ трогаются. Возраст — от archived_at.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeArchiveMaxAge, batchSize, dryRun,
				r.deps.Purger.PurgeArchivedStateHistory)
		case "purge_apply_task_register":
			// `max_age` тут семантически = grace после терминала apply_run
			// (см. docs/keeper/reaper.md). Поле общее со структурой ReaperRule.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeApplyTaskRegisterGrace, batchSize, dryRun,
				r.deps.Purger.PurgeApplyTaskRegister)
		case "reclaim_apply_runs":
			// Recovery-скан недо-доставленных Ward (ADR-027 amend, S4): только
			// `claimed` с истёкшим claim_expires_at (умер ДО отдачи Soul-у) →
			// `planned`. `dispatched` НЕ реклеймится — после отдачи прогоном
			// владеет Soul, пере-claim = двойной apply. `stale_after` — формальный
			// lease-аргумент (в предикат не входит, recovery сравнивает
			// claim_expires_at < NOW() напрямую). Правило по дефолту ВЫКЛЮЧЕНО —
			// включать только при attempt-fencing на приёме RunResult, иначе
			// recovery может конфликтовать со stale-результатом (docs/keeper/reaper.md).
			r.runDurationRule(ctx, name, rule.StaleAfter, defaultReclaimApplyRunsLease, batchSize, dryRun,
				r.deps.Purger.ReclaimApplyRuns)
		case "scry_background":
			// Фоновое periodic drift-сканирование (ADR-031 Slice C). Default
			// OFF (через enabled: false выше) + opt-in; параметры
			// max_concurrent_in_flight / min_interval_per_incarnation резолвятся
			// внутри runScryBackground. Запускает per-incarnation goroutine-ы,
			// tick синхронно ждёт их завершения (см. docstring).
			r.runScryBackground(ctx, name, rule, batchSize, dryRun, r.deps.Scry)
		case "archive_state_history":
			// ADR-Q19 retention (PM-решение, 2026-05): soft-delete активных
			// state_history-снимков сверх N последних на incarnation, опц. с
			// защитой version-bump (scenario='migration'). Не вписывается в
			// duration/statuses-runner-ы (нет max_age/stale_after; параметры —
			// integer N + bool keep_version_bump), поэтому отдельный runner.
			r.runArchiveStateHistory(ctx, name, rule, batchSize, dryRun)
		case "purge_orphan_push_runs":
			// Variant C push-orchestrator (docs/keeper/push.md): in-flight
			// push-прогоны старше `max_age` (default 1h) переводятся в `cancelled`
			// с пометкой `orphan_purged: true` в summary. Один LIST + per-row
			// UPDATE; single-winner-guard (WHERE status IN pending/running)
			// против гонки с реальным MarkTerminal. nil OrphanPushRuns → правило
			// деградирует с warn-ом (push wire-up ещё не подключён).
			if r.deps.OrphanPushRuns == nil {
				r.deps.Logger.Warn("reaper: purge_orphan_push_runs пропущено — OrphanPushRuns не сконфигурирован",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOrphanPushRunsMaxAge, batchSize, dryRun,
				r.deps.OrphanPushRuns.Run)
		case "reap_orphan_vault_keys":
			// Cross-store reconcile (report-only, GATE-2): находит приватники
			// подписи Sigil в Vault без строки в sigil_signing_keys и ТОЛЬКО
			// считает/метрит/логирует их — ничего не удаляет. `max_age`
			// семантически = grace по возрасту Vault-секрета (отсекает гонку с
			// Introduce write-before-PG-commit), как `purge_apply_task_register`
			// использует MaxAge-as-grace. По дефолту ВЫКЛЮЧЕНО (нужен Vault и
			// list-права на secret/metadata/keeper/sigil-keys/*). См.
			// docs/keeper/reaper.md.
			r.runDurationRule(ctx, name, rule.MaxAge, defaultReapOrphanVaultKeysGrace, batchSize, dryRun,
				r.deps.Purger.ReportOrphanVaultKeys)
		case "purge_old_errands":
			// Errand TTL retention (ADR-033, docs/keeper/reaper.md). `DELETE FROM
			// errands WHERE ttl_at < NOW()` — TTL зашит в строку при INSERT-е
			// dispatcher-ом (default 7д через errand.TTLDefault), `max_age` правила
			// в предикат НЕ входит (формальный аргумент для общего runner-а). nil
			// OldErrands → правило деградирует с warn-ом (errand wire-up ещё не
			// подключён — single-keeper dev без errand-стека).
			if r.deps.OldErrands == nil {
				r.deps.Logger.Warn("reaper: purge_old_errands пропущено — OldErrands не сконфигурирован",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOldErrandsMaxAge, batchSize, dryRun,
				r.deps.OldErrands.Run)
		case "purge_orphan_ephemeral_tidings":
			// Снос осиротевших ephemeral-Tiding-ов (ADR-052(g) amendment N2,
			// docs/keeper/reaper.md). Терминал Voyage должен унести свои разовые
			// подписки; это правило — страховка с grace-периодом. `max_age`
			// семантически = grace ПОСЛЕ терминала (ВХОДИТ в предикат, parity
			// `purge_apply_task_register`): снос раньше окна tap-consumer-а опередил
			// бы доставку терминального уведомления (dispatcher асинхронен,
			// ADR-052(c)). nil OrphanEphemeralTidings → правило деградирует с warn-ом
			// (herald wire-up может быть выключен).
			if r.deps.OrphanEphemeralTidings == nil {
				r.deps.Logger.Warn("reaper: purge_orphan_ephemeral_tidings пропущено — OrphanEphemeralTidings не сконфигурирован",
					slog.String("rule", name))
				continue
			}
			r.runDurationRule(ctx, name, rule.MaxAge, defaultPurgeOrphanEphemeralTidingsGrace, batchSize, dryRun,
				r.deps.OrphanEphemeralTidings.Run)
		default:
			r.deps.Logger.Warn("reaper: unknown rule name, skipping",
				slog.String("rule", name),
			)
		}
	}

	r.dispatchReclaimVoyages(ctx, cfg, batchSize, dryRun)
}

// dispatchReclaimVoyages исполняет правило reclaim_voyages с default-ON
// path-defaulting (ADR-043 §8): правило работает, если ключ отсутствует в
// cfg.Reaper.Rules ИЛИ присутствует с Enabled:true; пропускается ТОЛЬКО при
// явном Enabled:false. Вызывается из dispatch ОДИН раз после основного цикла
// (откуда reclaim_voyages исключён через `continue`), поэтому двойного запуска
// при Enabled:true нет.
//
// Recovery-скан протухших Voyage-claim-ов (ADR-043 S4, docs/keeper/reaper.md):
// `status='running' AND claim_expires_at < NOW()` → `pending` для пере-claim
// другим Keeper-инстансом, attempt++ (fencing-epoch). `stale_after` — формальный
// lease-аргумент (в SQL-предикат НЕ входит, lease зашит в claim_expires_at); при
// отсутствии ключа в map используется defaultReclaimVoyagesLease. nil
// VoyageReclaim → правило деградирует с warn-ом (voyage wire-up может быть
// выключен).
//
// Default-ON безопасен: дубль-commit отсекает CAS-ownership-guard в
// voyage.Finalize (WHERE claimed_by_kid=$2 → ErrLeaseLost для stale-воркера).
func (r *Runner) dispatchReclaimVoyages(ctx context.Context, cfg *config.KeeperConfig, batchSize int, dryRun bool) {
	const name = "reclaim_voyages"
	rule, ok := cfg.Reaper.Rules[name]
	if ok && !rule.Enabled {
		return
	}
	if r.deps.VoyageReclaim == nil {
		r.deps.Logger.Warn("reaper: reclaim_voyages пропущено — VoyageReclaim не сконфигурирован",
			slog.String("rule", name))
		return
	}
	r.runDurationRule(ctx, name, rule.StaleAfter, defaultReclaimVoyagesLease, batchSize, dryRun,
		r.deps.VoyageReclaim.Run)
}

// runDurationRule — общий runner для правил с сигнатурой
// `(ctx, duration, batchSize) → (count, err)`. Извлекает duration из
// raw-строки cfg, обрабатывает dry_run и логирует унифицированно.
//
// `rawDuration` — `rule.MaxAge` (для большинства правил) или
// `rule.StaleAfter` (для `mark_disconnected`); selector делает caller
// при выборе runner-а.
func (r *Runner) runDurationRule(
	ctx context.Context,
	ruleName string,
	rawDuration string,
	defaultDuration time.Duration,
	batchSize int,
	dryRun bool,
	call func(context.Context, time.Duration, int) (int64, error),
) {
	duration, err := parseRuleDuration(rawDuration, defaultDuration)
	if err != nil {
		r.deps.Logger.Warn("reaper: invalid duration, using default",
			slog.String("rule", ruleName),
			slog.String("raw", rawDuration),
			slog.Any("error", err),
			slog.Duration("default", defaultDuration),
		)
		duration = defaultDuration
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Duration("duration", duration),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := call(ctx, duration, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Duration("duration", duration),
		slog.Int("batch_size", batchSize),
	)
}

// runStatusesRule — общий runner для правил со statuses[]-фильтром
// (`purge_souls`, `purge_old_seeds`). Аналог runDurationRule, но
// дополнительно резолвит statuses (cfg → default, если пусто).
func (r *Runner) runStatusesRule(
	ctx context.Context,
	ruleName string,
	rawStatuses []string,
	defaultStatuses []string,
	rawMaxAge string,
	defaultMaxAge time.Duration,
	batchSize int,
	dryRun bool,
	call func(context.Context, []string, time.Duration, int) (int64, error),
) {
	statuses := rawStatuses
	if len(statuses) == 0 {
		statuses = defaultStatuses
	}

	maxAge, err := parseRuleDuration(rawMaxAge, defaultMaxAge)
	if err != nil {
		r.deps.Logger.Warn("reaper: invalid max_age, using default",
			slog.String("rule", ruleName),
			slog.String("raw", rawMaxAge),
			slog.Any("error", err),
			slog.Duration("default", defaultMaxAge),
		)
		maxAge = defaultMaxAge
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Any("statuses", statuses),
			slog.Duration("max_age", maxAge),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := call(ctx, statuses, maxAge, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Any("statuses", statuses),
		slog.Duration("max_age", maxAge),
		slog.Int("batch_size", batchSize),
	)
}

// runArchiveStateHistory — runner правила `archive_state_history` (ADR-Q19
// retention). У правила параметры — integer N + bool keep_version_bump
// (см. ReaperRule.KeepLastN / KeepVersionBumpSnapshots), несовместимые с
// runDurationRule/runStatusesRule, поэтому отдельная функция.
//
// Дефолты (cfg `*int`/`*bool` = nil → не задано):
//   - keep_last_n = 50 ([defaultArchiveStateHistoryKeepLastN])
//   - keep_version_bump = true ([defaultArchiveStateHistoryKeepVersionBump])
func (r *Runner) runArchiveStateHistory(ctx context.Context, ruleName string, rule config.ReaperRule, batchSize int, dryRun bool) {
	keepLastN := defaultArchiveStateHistoryKeepLastN
	if rule.KeepLastN != nil {
		keepLastN = *rule.KeepLastN
	}
	if keepLastN <= 0 {
		r.deps.Logger.Warn("reaper: archive_state_history keep_last_n must be > 0, using default",
			slog.String("rule", ruleName),
			slog.Int("raw", keepLastN),
			slog.Int("default", defaultArchiveStateHistoryKeepLastN),
		)
		keepLastN = defaultArchiveStateHistoryKeepLastN
	}

	keepVersionBump := defaultArchiveStateHistoryKeepVersionBump
	if rule.KeepVersionBumpSnapshots != nil {
		keepVersionBump = *rule.KeepVersionBumpSnapshots
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Int("keep_last_n", keepLastN),
			slog.Bool("keep_version_bump", keepVersionBump),
			slog.Int("batch_size", batchSize),
		)
		return
	}

	start := time.Now()
	affected, err := r.deps.Purger.ArchiveStateHistory(ctx, keepLastN, keepVersionBump, batchSize)
	r.deps.Metrics.ObserveRule(ruleName, affected, err, time.Since(start))
	if err != nil {
		r.deps.Logger.Error("reaper: rule failed",
			slog.String("rule", ruleName),
			slog.Any("error", err),
		)
		return
	}
	r.deps.Logger.Info("reaper: rule applied",
		slog.String("rule", ruleName),
		slog.Int64("affected", affected),
		slog.Int("keep_last_n", keepLastN),
		slog.Bool("keep_version_bump", keepVersionBump),
		slog.Int("batch_size", batchSize),
	)
}

// reaperLockTTL/reaperInterval — селекторы для parseDurationOr. Вынесены
// в типизированные функции, чтобы не дублировать nil-check на `Reaper`
// и не плодить inline-условия.
func reaperLockTTL(r *config.KeeperReaper) string  { return r.LockTTL }
func reaperInterval(r *config.KeeperReaper) string { return r.Interval }

// parseDurationOr читает duration-строку из cfg.Reaper через selector.
// При nil-Reaper или пустой строке возвращает fallback. На invalid-формат
// тоже возвращает fallback (semantic-валидация keeper.yml уже отвергает
// невалидные duration-ы; на хорошо валидированном конфиге сюда не попадаем).
func parseDurationOr(r *config.KeeperReaper, fallback time.Duration, sel func(*config.KeeperReaper) string) time.Duration {
	if r == nil {
		return fallback
	}
	d, err := parseRuleDuration(sel(r), fallback)
	if err != nil {
		return fallback
	}
	return d
}

// parseRuleDuration — обёртка над [config.ParseDuration] с дефолтом для
// пустой строки. Единая convention `duration` Soul Stack (Go-duration или
// `<N>d` с overflow-guard-ом) обеспечивается shared/config.
func parseRuleDuration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	return config.ParseDuration(s)
}

// ResolveMarkDisconnectedStale возвращает фактический `stale_after` правила
// `mark_disconnected` (cfg-значение либо [defaultMarkDisconnectedStale]).
// Единая точка истины порога disconnect — нужна вне Reaper-а: EventStream-flush
// `last_seen_at` выводит свой throttle-интервал из этого порога, чтобы flush
// заведомо был чаще, чем Reaper метит стрим disconnected (см. ADR-006(a)).
//
// Невалидная cfg-строка (semantic-validate её отверг бы на старте) → дефолт.
func ResolveMarkDisconnectedStale(cfg *config.KeeperReaper) time.Duration {
	if cfg == nil {
		return defaultMarkDisconnectedStale
	}
	rule, ok := cfg.Rules["mark_disconnected"]
	if !ok {
		return defaultMarkDisconnectedStale
	}
	d, err := parseRuleDuration(rule.StaleAfter, defaultMarkDisconnectedStale)
	if err != nil {
		return defaultMarkDisconnectedStale
	}
	return d
}
