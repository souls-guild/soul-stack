// Пакет acolyte — пул воркеров исполнения apply на Keeper-инстансе
// (ADR-027, Acolyte). Заменяет одиночную run-goroutine инстанса-владельца:
// каждый Acolyte периодически опрашивает очередь planned-заданий и атомарно
// клеймит их (Ward) через `FOR UPDATE SKIP LOCKED`. Любой инстанс через свой
// пул подхватывает любое задание → исполнение распределено по кластеру.
//
// Пул claim-agnostic: он лишь периодически дёргает инъектированный claim-callback
// (по умолчанию no-op), не зная про applyrun/scenario. Реальный claim/render/
// dispatch живёт в [scenario.ClaimRunner], который wire-up (setupAcolyte, slice
// 1.4.4) подключает через [Pool.SetClaim].
//
// Lifecycle построен по образцу keeper/internal/reaper/runner.go и
// keeper/internal/scenario/runner.go: graceful-shutdown через
// [sync.WaitGroup] + ctx-cancel. Останов двухстадийный (graceful-drain пула Acolyte,
// ADR-027 Phase 2): сперва drain-сигнал «больше не claim-ить» гасит loop, давая
// уже идущим in-flight-claim-ам добежать в пределах grace; не уложившиеся —
// прерываются отменой claim-ctx, их Ward остаётся в БД (claimed/running) и
// подбирается recovery-сканом (ADR-027(i)) — НЕ форсится commit/rollback.
package acolyte

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// defaultPollInterval — период poll-tick-а воркера при опущенном
// cfg.PollInterval. Acolyte всё равно периодически сканирует planned-задания,
// поэтому poll — fallback к Summons-сигналу (ADR-027(a)): даже при потере
// pub/sub-сигнала задание подхватится на ближайшем тике. Значение умеренное:
// достаточно частое, чтобы failover после смерти владельца происходил в
// пределах единиц секунд, и достаточно редкое, чтобы не флудить PG пустыми
// claim-запросами на простаивающем кластере.
const defaultPollInterval = 2 * time.Second

// defaultDrainGrace — окно graceful-drain пула Acolyte (ADR-027 Phase 2)
// при опущенном cfg.DrainGrace. От сигнала «больше не claim-ить» до жёсткой
// отмены claim-ctx у не успевших in-flight-воркеров. Значение умеренное:
// достаточно, чтобы уже начатый claim (render → MarkDispatched → SendApply)
// добежал до конца на здоровом PG/Soul, и не настолько большое, чтобы тормозить
// SIGTERM-выход узла на зависшем in-flight (его claim переживёт рестарт —
// останется в БД, lease истечёт → подберёт recovery-скан, ADR-027(i)).
const defaultDrainGrace = 5 * time.Second

// hardStopTimeout — добавочная граница ожидания выхода воркеров ПОСЛЕ отмены
// claim-ctx (когда grace уже исчерпан). Прерванный по ctx claim разматывает
// стек быстро; этот запас лишь страхует от warn про leak на медленном unwind-е.
const hardStopTimeout = 5 * time.Second

// ClaimFunc — DI-шов под claim-логику. Вызывается воркером на каждом poll-tick-е
// (и на Summons-wake). По умолчанию [noopClaim] — пул крутится вхолостую, пока
// caller не подключит реальный захват Ward. Wire-up (setupAcolyte, slice 1.4.4)
// задаёт сюда [scenario.ClaimRunner.Claim] (ClaimNext→RenderForHost→
// MarkDispatched→SendApply). Возвращаемая ошибка логируется воркером и не останавливает
// loop: сбой одного тика (например, временная недоступность PG) не роняет пул.
type ClaimFunc func(ctx context.Context) error

// noopClaim — дефолтный claim-callback, пока caller не подключит реальный через
// [Pool.SetClaim] (setupAcolyte, slice 1.4.4).
func noopClaim(context.Context) error { return nil }

// SummonsSubscriber — DI-шов под Redis-подписку на Summons-сигнал (ADR-027(a),
// slice 1.3). Поднимает подписку на топик `apply:summons`, вызывая onSignal на
// каждый полученный сигнал; возвращает io.Closer для graceful-стопа подписки.
//
// Абстракция (а не прямой импорт keeper/internal/redis) держит acolyte
// независимым от Redis-клиента: daemon на старте связывает её с
// redis.SubscribeSummons (callback = pool.Notify). Если подписчик не задан
// (Redis выключен или Acolytes>0 без кластер-режима) — пул работает на чистом
// poll-fallback-е, потеря Summons-ускорения задание не теряет.
type SummonsSubscriber func(ctx context.Context, onSignal func()) (io.Closer, error)

// Config — параметры пула. Заполняется из keeper.yml на старте (setupAcolyte:
// Workers ← `acolytes`, PollInterval ← `acolyte_poll_interval`,
// DrainGrace ← `acolyte_drain_grace`).
type Config struct {
	// Workers — число воркеров пула (`keeper.acolytes`). Caller поднимает Pool
	// только при Workers > 0 (feature-flag, см. setupAcolyte); конструктор
	// требует положительное значение и иначе возвращает ошибку.
	Workers int

	// PollInterval — период poll-tick-а воркера (`keeper.acolyte_poll_interval`).
	// Zero-value → [defaultPollInterval].
	PollInterval time.Duration

	// DrainGrace — окно graceful-drain в [Pool.Shutdown] (`keeper.acolyte_drain_grace`).
	// От сигнала «больше не claim-ить» до жёсткой отмены claim-ctx у не успевших
	// in-flight-воркеров. Zero-value → [defaultDrainGrace].
	DrainGrace time.Duration
}

// Deps — внешние зависимости пула. Logger обязателен; Claim инъектируется
// сеттером [Pool.SetClaim] после конструирования (slice 1.4), по умолчанию
// no-op — пул собирается и стартует независимо от готовности claim-логики.
//
// Summons — опциональный подписчик на Redis-сигнал planned-заданий (slice 1.3).
// nil → пул работает на poll-fallback-е без Summons-ускорения (Redis выключен).
type Deps struct {
	Logger  *slog.Logger
	Summons SummonsSubscriber
}

// Pool — пул из N Acolyte-воркеров. Один экземпляр на keeper-процесс.
type Pool struct {
	cfg    Config
	logger *slog.Logger

	// claim — захват Ward на тике. Защищён mu: SetClaim может быть вызван до
	// Start, но читается воркерами на каждом тике; mu снимает гонку.
	mu    sync.Mutex
	claim ClaimFunc

	// wake — буферизованный канал «появились planned-задания» (Summons-wake,
	// ADR-027(a)). [Pool.Notify] кладёт сигнал не блокируясь; воркер на нём
	// просыпается раньше следующего poll-tick-а. Redis pub/sub, питающий этот
	// канал, — slice 1.3: подписка на топик `apply:summons` поднимается в
	// [Pool.Start] и дёргает [Pool.Notify].
	wake chan struct{}

	// summons — DI-подписчик на Summons-сигнал; nil → только poll-fallback.
	// summonsCloser — handle активной подписки (закрывается в Shutdown).
	summons       SummonsSubscriber
	summonsCloser io.Closer

	// drain — drain-сигнал «больше не claim-ить» (graceful-drain пула Acolyte, ADR-027 Phase 2,
	// graceful-drain). Закрывается единожды [Pool.beginDrain] на Shutdown-е;
	// воркер, увидев его в select, выходит из loop-а БЕЗ запуска нового тика.
	// Уже идущий in-flight-тик при этом НЕ прерывается — он добегает до конца
	// (или до отмены claimCtx по истечении grace).
	drain     chan struct{}
	drainOnce sync.Once

	// claimCtx/claimCancel — ctx, под которым выполняется claim-callback
	// (НЕ worker-lifecycle-ctx). Разводит две стадии остановки: drain гасит
	// loop (новых claim-ов нет), а claimCancel прерывает уже идущий claim
	// только по истечении grace. Прерванный claim оставляет Ward в БД как есть
	// (claimed/running) — lease истечёт, задание подберёт recovery (ADR-027(i)),
	// двойного исполнения нет (fencing, ADR-027(g)). Инициализируются в Start.
	claimCtx    context.Context
	claimCancel context.CancelFunc

	// inflight — число воркеров, прямо сейчас исполняющих claim-callback.
	// На момент истечения grace в Shutdown его значение = число in-flight,
	// которые будут прерваны отменой claimCtx (для лога drain-итога).
	inflight atomic.Int64

	wg sync.WaitGroup
}

// NewPool проверяет cfg/deps и возвращает пул. На некорректные параметры —
// ошибка: это программная ошибка caller-а (setupAcolyte), не runtime-условие.
// Claim по умолчанию no-op; реальный захват подключается [Pool.SetClaim].
func NewPool(cfg Config, deps Deps) (*Pool, error) {
	if cfg.Workers <= 0 {
		return nil, errors.New("acolyte.NewPool: Workers must be > 0")
	}
	if deps.Logger == nil {
		return nil, errors.New("acolyte.NewPool: Logger is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.DrainGrace <= 0 {
		cfg.DrainGrace = defaultDrainGrace
	}
	return &Pool{
		cfg:     cfg,
		logger:  deps.Logger,
		claim:   noopClaim,
		summons: deps.Summons,
		// Буфер 1: коалесцируем всплеск Summons-сигналов в один wake —
		// воркеру достаточно одного «проснись и проверь очередь».
		wake:  make(chan struct{}, 1),
		drain: make(chan struct{}),
	}, nil
}

// SetClaim инъектирует claim-callback (slice 1.4 — applyrun.ClaimNext).
// Безопасно вызывать до Start; nil-callback игнорируется (остаётся no-op).
func (p *Pool) SetClaim(fn ClaimFunc) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	p.claim = fn
	p.mu.Unlock()
}

// Notify будит воркеров: «появились planned-задания» (Summons-wake). Не
// блокируется — при уже-взведённом сигнале лишний Notify коалесцируется.
// Питается Redis pub/sub-подписчиком (slice 1.3); пока — точка вызова для
// будущего wire-up-а.
func (p *Pool) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Start запускает cfg.Workers воркеров и сразу возвращается (async). Воркеры
// крутятся, пока ctx не отменится; завершение дожидается [Pool.Shutdown].
//
// Если задан Summons-подписчик (Deps.Summons), Start поднимает подписку на
// `apply:summons` с callback = [Pool.Notify]: пришедший сигнал будит воркеров
// раньше poll-tick-а. Сбой подписки best-effort — логируется warn-ом, пул
// продолжает работать на poll-fallback-е (потеря Summons-ускорения не теряет
// задание). Подписка живёт под переданным ctx и закрывается в [Pool.Shutdown].
func (p *Pool) Start(ctx context.Context) {
	// claimCtx — производный от Start-ctx ctx исполнения claim-callback-а.
	// Worker-lifecycle крутится под исходным ctx; claimCtx отменяется отдельно
	// (Shutdown по истечении grace), чтобы graceful-drain мог дать in-flight-у
	// добежать ДО жёсткой отмены, а не рубить его одновременно с loop-ом.
	p.claimCtx, p.claimCancel = context.WithCancel(ctx)

	if p.summons != nil {
		closer, err := p.summons(ctx, p.Notify)
		if err != nil {
			p.logger.Warn("acolyte: summons subscribe failed — poll-fallback only",
				slog.Any("error", err))
		} else {
			p.summonsCloser = closer
		}
	}

	for i := 0; i < p.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}
	p.logger.Info("acolyte: pool started",
		slog.Int("workers", p.cfg.Workers),
		slog.Duration("poll_interval", p.cfg.PollInterval),
		slog.Duration("drain_grace", p.cfg.DrainGrace),
		slog.Bool("summons", p.summonsCloser != nil),
	)
}

// beginDrain единожды закрывает drain-канал (drain-сигнал «больше не
// claim-ить») и будит воркеров, заблокированных на poll-tick-ожидании, чтобы
// они сразу увидели drain и вышли из loop-а. Идемпотентна (sync.Once): повторный
// Shutdown / гонка не паникуют на двойном close.
func (p *Pool) beginDrain() {
	p.drainOnce.Do(func() {
		close(p.drain)
		// Разбудить воркеров, висящих в select на poll-tick: на следующем
		// проходе они увидят закрытый drain и выйдут. Notify не блокируется.
		p.Notify()
	})
}

// Shutdown выполняет graceful-drain пула Acolyte (ADR-027 Phase 2):
//
//  1. Закрывает Summons-подписку (новые wake-сигналы бессмысленны).
//  2. beginDrain — воркеры перестают входить в НОВЫЕ claim-тики; уже идущий
//     in-flight-тик НЕ прерывается.
//  3. Ждёт выхода воркеров в пределах grace (cfg.DrainGrace либо более ранний
//     дедлайн переданного ctx). In-flight-тик, уложившийся в grace, завершается
//     штатно (claimed→dispatched или терминал — как обычный claim).
//  4. По истечении grace — claimCancel: прерывает claim-callback не успевших
//     in-flight-воркеров по ctx. Прерванный ДО отметки dispatched claim оставляет
//     свой Ward в БД КАК ЕСТЬ (claimed, attempt/lease не трогаются) — НЕ форсит
//     commit/rollback: lease истечёт, задание подберёт recovery-скан (ADR-027(i)),
//     fencing исключает двойное исполнение (ADR-027(g)). Затем добор выхода в
//     пределах [hardStopTimeout].
//
// Возвращает ctx.Err(), если переданный ctx истёк раньше штатного завершения
// (drain не уложился в окно). Лог-итог: сколько in-flight прервано по grace.
func (p *Pool) Shutdown(ctx context.Context) error {
	if p.summonsCloser != nil {
		if err := p.summonsCloser.Close(); err != nil {
			p.logger.Warn("acolyte: summons subscription close error", slog.Any("error", err))
		}
		p.summonsCloser = nil
	}

	p.beginDrain()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	// Окно grace: минимум из cfg.DrainGrace и дедлайна переданного ctx.
	grace := time.NewTimer(p.cfg.DrainGrace)
	defer grace.Stop()

	select {
	case <-done:
		// Все воркеры вышли в пределах grace — in-flight добежал штатно.
		p.logger.Info("acolyte: pool drained gracefully")
		return nil
	case <-grace.C:
		// grace исчерпан — прерываем не успевший in-flight по claimCtx.
	case <-ctx.Done():
		// Переданный ctx (15s shutdown-таймаут daemon-а) истёк раньше grace —
		// тоже переходим к жёсткой отмене.
	}

	interrupted := p.inflight.Load()
	if p.claimCancel != nil {
		p.claimCancel()
	}
	if interrupted > 0 {
		p.logger.Warn("acolyte: drain grace exceeded — in-flight claims aborted by ctx (Ward kept in DB for recovery)",
			slog.Int64("inflight_aborted", interrupted),
			slog.Duration("grace", p.cfg.DrainGrace),
		)
	}

	select {
	case <-done:
		return ctx.Err()
	case <-time.After(hardStopTimeout):
		p.logger.Warn("acolyte: workers did not stop within hard-stop timeout — leak suspected",
			slog.Duration("timeout", hardStopTimeout),
		)
		return ctx.Err()
	}
}

// worker — poll-tick loop одного Acolyte-а. На каждом тике (или Summons-wake)
// дёргает claim-callback. Выход — по ctx.Done (жёсткая отмена) ИЛИ по drain
// (drain: больше не claim-ить).
//
// Граф остановки двухстадийный: drain гасит loop (новые тики не стартуют), но
// уже идущий [Pool.tick] добегает до конца — его прерывает только claimCtx,
// отменяемый Shutdown-ом по истечении grace. Поэтому drain проверяется и в
// основном select (не входить в новый тик), и отдельной проверкой ПЕРЕД самим
// tick (canStartTick): между пробуждением по wake/poll и запуском claim drain
// мог уже взвестись.
func (p *Pool) worker(ctx context.Context, id int) {
	defer p.wg.Done()

	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.drain:
			// Drain: новых claim-ов не начинаем, выходим из loop-а.
			return
		case <-t.C:
			if p.canStartTick() {
				p.tick(p.claimCtx, id)
			}
		case <-p.wake:
			// Summons-wake (в т.ч. beginDrain дёргает Notify): проверяем drain
			// перед запуском тика — на drain-wake тик не стартуем, идём на выход.
			if p.canStartTick() {
				p.tick(p.claimCtx, id)
			}
		}
	}
}

// canStartTick сообщает, можно ли начинать новый claim-тик: false, если drain
// уже взведён. Закрывает гонку «проснулись по wake, но между select и tick
// встал drain».
func (p *Pool) canStartTick() bool {
	select {
	case <-p.drain:
		return false
	default:
		return true
	}
}

// tick выполняет один проход claim под claimCtx. Ошибка claim-callback-а
// логируется и НЕ останавливает воркер (best-effort: сбой одного тика не роняет
// пул). Инкремент/декремент inflight оборачивает вызов: на момент истечения
// grace в Shutdown его значение = число in-flight, прерываемых отменой claimCtx.
func (p *Pool) tick(ctx context.Context, id int) {
	p.mu.Lock()
	claim := p.claim
	p.mu.Unlock()

	p.inflight.Add(1)
	err := claim(ctx)
	p.inflight.Add(-1)

	if err != nil {
		// ctx.Canceled — норма на жёсткой отмене drain-grace (claimCtx отменён):
		// in-flight прерван штатно, Ward остаётся в БД для recovery, не шумим.
		if errors.Is(err, context.Canceled) {
			return
		}
		p.logger.Warn("acolyte: claim tick failed",
			slog.Int("worker", id),
			slog.Any("error", err),
		)
	}
}
