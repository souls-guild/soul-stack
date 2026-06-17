package beacon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Scheduler — per-process планировщик активного набора Vigil (ADR-030 S1).
//
// Один Scheduler на soul-демон, живёт вне reconnect-loop (как ApplyRunner):
// набор Vigil и их last-state переживают разрыв/переустановку EventStream-а.
// Безвреден без Vigil — до первого VigilSnapshot ничего не делает.
//
// Жизненный цикл одного Vigil:
//   - на (ре)старте Vigil scheduler гоняет проверку каждые VigilDef.interval;
//   - первая проверка устанавливает baseline БЕЗ Portent (барьер против шторма
//     при рестарте Soul-а / появлении нового Vigil);
//   - последующая смена State vs last → эмиссия Portent (edge-triggered);
//   - совпадение State с last → ничего (no-change);
//   - Vigil, исчезнувший из нового VigilSnapshot, останавливается и его
//     last-state забывается (disable/удаление без перезапуска Soul-а).
//
// Готовый Portent кладётся в [Scheduler.Portents] — буферизованный канал, из
// которого единственный writer EventStream-а (select-loop handleSession)
// забирает события и шлёт через сессию. Сам scheduler сессию не держит и Send
// не делает: StreamSession не concurrent-safe для Send (ADR-012), поэтому все
// FromSoul идут из одного writer-loop-а (как soulprintTick).
type Scheduler struct {
	registry BeaconLookup
	sid      string
	logger   *slog.Logger
	metrics  *BeaconMetrics

	// now / newTicker — инъекция времени для детерминированных тестов. В
	// production — time.Now / реальный тикер.
	now       func() time.Time
	newTicker func(time.Duration) ticker

	portents chan *keeperv1.PortentEvent

	mu      sync.Mutex
	running map[string]*vigilRun // name → активный прогон Vigil
	stopped bool
}

// vigilRun — один запущенный Vigil: его горутина + cancel.
type vigilRun struct {
	def    *keeperv1.VigilDef
	cancel context.CancelFunc
	done   chan struct{}
}

// ticker — узкая абстракция над time.Ticker для подмены в тестах.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// SchedulerConfig — параметры [NewScheduler].
type SchedulerConfig struct {
	// Registry — резолв beacon-ов по имени. Не nil. В production —
	// [CompositeRegistry] (core + plugin); в тестах — статический [Registry] /
	// fake-Lookup.
	Registry BeaconLookup
	// SID — кладётся в PortentEvent.sid (echo, ADR-012(i)).
	SID string
	// Logger — nil → slog.Default.
	Logger *slog.Logger
	// PortentBuffer — ёмкость канала Portents. 0 → defaultPortentBuffer.
	PortentBuffer int
	// Metrics — soul_beacon_*-дескриптор (ADR-024 S4). nil → инструментация
	// выключена (nil-safe Observe*-методы — no-op), как Metrics в ApplyRunner.
	Metrics *BeaconMetrics
}

const defaultPortentBuffer = 64

// NewScheduler собирает scheduler. Не запускает ни одного Vigil — набор едет
// через [Scheduler.Apply].
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	buf := cfg.PortentBuffer
	if buf <= 0 {
		buf = defaultPortentBuffer
	}
	return &Scheduler{
		registry:  cfg.Registry,
		sid:       cfg.SID,
		logger:    logger,
		metrics:   cfg.Metrics,
		now:       time.Now,
		newTicker: func(d time.Duration) ticker { return realTicker{t: time.NewTicker(d)} },
		portents:  make(chan *keeperv1.PortentEvent, buf),
		running:   make(map[string]*vigilRun),
	}
}

// Portents — канал готовых Portent. Читается единственным writer-ом
// EventStream-а (select-loop handleSession), который шлёт их в текущую сессию.
// Канал per-process: переживает reconnect, поэтому событие, поднятое между
// сессиями, не теряется (буфер) и уедет в следующую сессию.
//
// nil-receiver-safe: на nil-scheduler (тестовая обвязка без beacon-контура)
// возвращает nil-канал — select на нём блокируется навсегда, что для
// handleSession означает «Portent никогда не придёт».
func (s *Scheduler) Portents() <-chan *keeperv1.PortentEvent {
	if s == nil {
		return nil
	}
	return s.portents
}

// Apply применяет VigilSnapshot как ReplaceAll (ADR-030): набор из snapshot
// становится единственным активным. Vigil:
//   - новый (нет в running) — запускается (baseline без Portent);
//   - существующий с тем же check+interval+params — оставляется как есть
//     (last-state сохраняется, рестарт не нужен);
//   - существующий с изменившимся определением — перезапускается (новый baseline);
//   - отсутствующий в snapshot — останавливается и забывается.
//
// ctx — родительский контекст демона: при его отмене все Vigil-горутины гаснут.
func (s *Scheduler) Apply(ctx context.Context, defs []*keeperv1.VigilDef) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}

	wanted := make(map[string]*keeperv1.VigilDef, len(defs))
	for _, d := range defs {
		if d.GetName() == "" {
			s.logger.Warn("beacon: vigil без имени в snapshot пропущен")
			continue
		}
		wanted[d.GetName()] = d
	}

	// Останавливаем Vigil, которых нет в новом наборе либо чьё определение
	// изменилось (нужен перезапуск с новым baseline).
	for name, run := range s.running {
		newDef, keep := wanted[name]
		if keep && sameVigil(run.def, newDef) {
			delete(wanted, name) // уже запущен в актуальном виде — не трогаем
			continue
		}
		s.stopRun(name, run)
	}

	// Запускаем оставшиеся (новые + изменённые).
	for name, def := range wanted {
		s.startRun(ctx, name, def)
	}
}

// Stop останавливает все Vigil и переводит scheduler в терминальное состояние:
// последующие Apply — no-op. Идемпотентен. Канал Portents НЕ закрывается —
// его читатель (handleSession) завершается по своему ctx, а закрытие канала
// продюсером при возможных in-flight Send-ах привело бы к панике/гонке.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for name, run := range s.running {
		s.stopRun(name, run)
	}
}

// stopRun — под s.mu: отменяет горутину Vigil и ждёт её завершения, затем
// забывает запись (и тем самым last-state).
func (s *Scheduler) stopRun(name string, run *vigilRun) {
	run.cancel()
	<-run.done
	delete(s.running, name)
}

// startRun — под s.mu: запускает горутину Vigil. Неизвестный check логируется и
// Vigil не запускается (не валит scheduler — кривой реестр на Keeper не должен
// ронять Soul).
func (s *Scheduler) startRun(ctx context.Context, name string, def *keeperv1.VigilDef) {
	b, ok := s.registry.Lookup(def.GetCheck())
	if !ok {
		s.logger.Warn("beacon: неизвестный check, vigil не запущен",
			slog.String("vigil", name),
			slog.String("check", def.GetCheck()),
		)
		return
	}
	interval, err := config.ParseDuration(def.GetInterval())
	if err != nil || interval <= 0 {
		s.logger.Warn("beacon: невалидный interval, vigil не запущен",
			slog.String("vigil", name),
			slog.String("interval", def.GetInterval()),
			slog.Any("error", err),
		)
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	run := &vigilRun{def: def, cancel: cancel, done: make(chan struct{})}
	s.running[name] = run
	go s.loop(runCtx, run, b, interval)
}

// loop — горутина одного Vigil. Каждый interval гоняет Check; baseline на первой
// проверке без Portent, далее edge-triggered.
func (s *Scheduler) loop(ctx context.Context, run *vigilRun, b Beacon, interval time.Duration) {
	defer close(run.done)

	tk := s.newTicker(interval)
	defer tk.Stop()

	var (
		last         State
		haveBaseline bool
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			state, data, err := b.Check(ctx, run.def.GetParams())
			if err != nil {
				// Ошибка проверки ≠ смена состояния хоста: baseline/last не
				// трогаем, Portent не эмитим. Лог — для диагностики.
				s.logger.Warn("beacon: проверка завершилась ошибкой",
					slog.String("vigil", run.def.GetName()),
					slog.String("check", run.def.GetCheck()),
					slog.Any("error", err),
				)
				continue
			}
			if !haveBaseline {
				last = state
				haveBaseline = true
				s.logger.Debug("beacon: baseline установлен (без Portent)",
					slog.String("vigil", run.def.GetName()),
					slog.String("state", state),
				)
				continue
			}
			if state == last {
				continue // no-change
			}
			last = state
			s.emit(ctx, run.def, state, data)
		}
	}
}

// emit формирует Portent и кладёт в канал. Неблокирующая отправка: если буфер
// полон (writer-loop отстаёт / нет активной сессии надолго), событие дропается с
// warn — иначе Vigil-горутина залипла бы и пропустила последующие смены. Дроп
// edge-triggered события — потеря одного перехода; следующая смена State снова
// поднимет Portent.
//
// V5-1 (ADR-030 amendment 2026-05-26): заполняем ОБЕ ветки — Data (deprecated)
// и Payload (typed oneof, по check-address). После 1-release deprecation period
// Data удаляется hard-cut (S5-final). Один раз на процесс — WARN о dual-write.
func (s *Scheduler) emit(ctx context.Context, def *keeperv1.VigilDef, state State, data *structpb.Struct) {
	ev := &keeperv1.PortentEvent{
		BeaconName:  def.GetName(),
		Data:        data,
		CollectedAt: timestamppb.New(s.now()),
		Sid:         s.sid,
	}
	fillTypedPayload(ev, def.GetCheck(), data)
	if ev.GetPayload() != nil {
		emitDeprecationWarnOnce(s.logger)
	}
	select {
	case s.portents <- ev:
		s.logger.Info("beacon: portent поднят (смена состояния)",
			slog.String("vigil", def.GetName()),
			slog.String("state", state),
		)
	case <-ctx.Done():
	default:
		s.metrics.ObservePortentDropped()
		s.logger.Warn("beacon: канал portent переполнен, событие отброшено",
			slog.String("vigil", def.GetName()),
			slog.String("state", state),
		)
	}
}

// sameVigil — определения Vigil эквивалентны (не нужен перезапуск): совпадают
// check, interval и params. Сравнение params — через proto-эквивалентность
// Struct (см. structEqual).
func sameVigil(a, b *keeperv1.VigilDef) bool {
	return a.GetCheck() == b.GetCheck() &&
		a.GetInterval() == b.GetInterval() &&
		structEqual(a.GetParams(), b.GetParams())
}

// structEqual — proto-эквивалентность двух Struct (оба nil → равны). Нужен,
// чтобы повторный VigilSnapshot с тем же определением не перезапускал Vigil
// (что сбросило бы baseline и подавило бы реальное событие).
func structEqual(a, b *structpb.Struct) bool {
	return proto.Equal(a, b)
}
