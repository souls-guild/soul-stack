// Package auditmulti — multi-writer fan-out [audit.Writer] для
// dual-write audit-pipeline-а (ADR-022(f)).
//
// Архитектурное решение write-policy (M0.4.1b):
//
//   - primary — синхронный, источник правды (Postgres-impl
//     `keeper/internal/auditpg`). Failure из primary возвращается caller-у
//     **до** запуска secondary-writes — inconsistent state (запись только
//     в OTel) недопустим.
//   - secondaries — асинхронные, best-effort (OTel-impl
//     `keeper/internal/auditotel` и любые будущие back-end-ы). Failure из
//     secondary логируется через [slog.Warn] и НЕ влияет на возвращаемое
//     значение Write.
//   - shutdown — graceful drain через [sync.WaitGroup] + bounded timeout
//     ([WithShutdownDrain], default 5s). Caller вызывает [Writer.Close],
//     обычно из основного shutdown-hook-а `cmd/keeper`.
//
// Event передаётся в secondary-writes через **глубокую копию** payload-а:
// иначе concurrent-чтение Payload-карты из multiple goroutines = data race.
// Маскировка секретов делегирована writer-ам (каждый прогоняет
// [audit.MaskSecrets] на своей стороне) — duplicate work осознан: писать
// маскировку в общую точку = протекание знания о storage formats в
// multi-writer.
//
// Context detach. Secondary-writers получают [context.WithoutCancel] от
// caller-ctx: caller (HTTP-handler / RPC-handler) обычно отменяет
// собственный context сразу после возврата Write, что для best-effort
// async-fan-out недопустимо (отменит OTel-export в полёте, потеряет
// debug-данные). Caller отвечает за shutdown через [Writer.Close],
// **не** через ctx-cancel.
package auditmulti

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// defaultShutdownDrain — bound для ожидания inflight secondary-writes при
// [Writer.Close]. Подобран по характеру OTel-export-а
// (BatchSpanProcessor flush ~1-3s в типовых конфигах); 5s — запас под
// connection-timeout-ы экспортёров.
const defaultShutdownDrain = 5 * time.Second

// Writer — multi-writer fan-out, удовлетворяющий [audit.Writer]. Один
// экземпляр на Keeper-процесс; safe for concurrent use.
//
// Возврат конкретного типа (не `audit.Writer`) — намеренный: caller-у
// нужен доступ к [Writer.Close] для graceful drain. Это стандартный
// Go-pattern «return concrete, accept interface».
type Writer struct {
	primary     audit.Writer
	secondaries []audit.Writer
	logger      *slog.Logger

	drain time.Duration

	// mu закрывает критическую секцию «проверка closed → wg.Add → запуск
	// goroutine» в Write и зеркальную секцию в Close. Без неё между
	// `select <-w.closed` и `wg.Add(1)` остаётся race-window, в которое
	// Close может закрыть канал и вернуться раньше, чем стартует
	// secondary-goroutine (ADR-022(f), review.C major-1).
	mu     sync.Mutex
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// Option — конфигурационный helper для [New].
type Option func(*Writer)

// WithShutdownDrain устанавливает максимальное время ожидания pending
// secondaries при [Writer.Close]. Значения ≤0 игнорируются — drain
// остаётся default 5s. Если caller хочет «no-wait» семантику,
// использовать `WithShutdownDrain(time.Microsecond)` либо
// `Close(ctx)` с уже отменённым context-ом.
func WithShutdownDrain(d time.Duration) Option {
	return func(w *Writer) {
		if d > 0 {
			w.drain = d
		}
	}
}

// WithLogger подставляет [slog.Logger] для warning-ов об secondary-failures
// и shutdown-timeout-е. Default — `slog.Default()`.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Writer) {
		if logger != nil {
			w.logger = logger
		}
	}
}

// New оборачивает primary (sync) + secondaries (async fan-out) в единый
// [audit.Writer]. Если `secondaries` пустой — поведение идентично прямому
// вызову primary.
//
// **Fail-fast валидация.** `nil`-primary или любой `nil` среди secondaries
// — это конфигурационная ошибка, видимая сразу при boot, а не при первой
// записи (review.C/qa.C major-3). New паникует с конкретным сообщением.
//
// Сигнатура: `[]audit.Writer` (slice) вместо variadic, чтобы не
// конфликтовать с variadic options. Caller строит slice явно:
//
//	mw := auditmulti.New(pgWriter, []audit.Writer{otelWriter},
//	    auditmulti.WithShutdownDrain(10*time.Second))
func New(primary audit.Writer, secondaries []audit.Writer, opts ...Option) *Writer {
	if primary == nil {
		panic("auditmulti.New: primary writer is nil")
	}
	for i, sec := range secondaries {
		if sec == nil {
			panic(fmt.Sprintf("auditmulti.New: secondaries[%d] is nil", i))
		}
	}
	w := &Writer{
		primary:     primary,
		secondaries: secondaries,
		logger:      slog.Default(),
		drain:       defaultShutdownDrain,
		closed:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Write реализует [audit.Writer]. См. package-doc для policy.
//
// Поведение после [Writer.Close]: primary продолжает работать (graceful
// drain последних событий перед polished shutdown), secondary-fan-out
// пропускается.
func (w *Writer) Write(ctx context.Context, event *audit.Event) error {
	// Nil-event — defensive отсечка. Контракт shared/audit допускает
	// валидацию на writer-уровне; в multi-writer-е важнее: иначе primary
	// получит nil и поведение становится impl-specific.
	if event == nil {
		return nil
	}

	// Primary — sync, без mutex: его собственный data race не нарушает
	// инвариантов multi-writer-а.
	if err := w.primary.Write(ctx, event); err != nil {
		// Primary-fail = инвариант аудита нарушен. Secondaries не
		// запускаются — иначе в OTel окажется событие, которого нет в
		// audit_log, и читатель не сможет cross-check-нуть.
		return err
	}

	if len(w.secondaries) == 0 {
		return nil
	}

	// Critical section: проверка closed → wg.Add → запуск goroutine
	// должна быть атомарна относительно Close. Без mutex Close может
	// закрыть канал между нашим select-ом и wg.Add(1), вернуть успех
	// (wg.Wait моментально), а secondary-goroutine стартанёт после
	// возврата Close — контракт «inflight завершились» сломан.
	w.mu.Lock()
	defer w.mu.Unlock()

	select {
	case <-w.closed:
		return nil
	default:
	}

	// Detach от caller-ctx: caller обычно отменяет ctx сразу после
	// возврата Write (HTTP-handler закрывает request-scope), а наши
	// secondary-writes — best-effort, должны успеть отработать.
	// Shutdown — через Close(), а не cancel.
	detached := context.WithoutCancel(ctx)

	for _, sec := range w.secondaries {
		evCopy := cloneEvent(event)
		w.wg.Add(1)
		go func(s audit.Writer, ev *audit.Event, c context.Context) {
			defer w.wg.Done()
			if err := s.Write(c, ev); err != nil {
				w.logger.Warn(
					"audit secondary write failed",
					slog.String("event_type", string(ev.EventType)),
					slog.String("audit_id", ev.AuditID),
					slog.String("error", err.Error()),
				)
			}
		}(sec, evCopy, detached)
	}
	return nil
}

// Close дожидается завершения inflight secondary-writes (или истечения
// shutdown-drain-а — см. [WithShutdownDrain]). Идемпотентен: повторные
// вызовы сразу возвращают nil. После Close новые Write-ы продолжают
// писать в primary, но secondary-fan-out не запускается (защита от
// utilizing stopped processors).
//
// При истечении drain pending secondary-goroutine **продолжают
// исполнение асинхронно**. Caller, владеющий ресурсами secondaries
// (например, OTel TracerProvider), **не должен** освобождать их сразу
// после возврата Close с таймаутом — типовой паттерн: после
// `multi.Close(ctx)` вызвать собственный `tracerProvider.Shutdown(ctx)`
// со своим bounded таймаутом, который и сделает finalize батчей.
//
// Возврат:
//   - nil — все inflight завершились в пределах drain-а.
//   - error("audit: secondary drain timeout") — drain истёк.
//   - ctx.Err() — caller отменил context.
func (w *Writer) Close(ctx context.Context) error {
	// Закрываем канал под mutex-ом, чтобы любой Write, уже взявший mu,
	// сначала отработал свой wg.Add (и Close-у будет что ждать), а
	// следующий Write увидит закрытый канал и пропустит fan-out.
	w.once.Do(func() {
		w.mu.Lock()
		close(w.closed)
		w.mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	deadline := time.NewTimer(w.drain)
	defer deadline.Stop()

	select {
	case <-done:
		return nil
	case <-deadline.C:
		w.logger.Warn(
			"audit multi-writer shutdown drain timed out",
			slog.Duration("max_wait", w.drain),
		)
		return errors.New("audit: secondary drain timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cloneEvent делает глубокую копию для безопасной передачи в
// goroutine-secondary. Payload — единственное shared mutable; остальные
// поля — value-types (`time.Time`, строки, enum-cast-ы).
func cloneEvent(ev *audit.Event) *audit.Event {
	if ev == nil {
		return nil
	}
	out := *ev
	out.Payload = clonePayload(ev.Payload)
	return &out
}

func clonePayload(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue глубоко копирует только `map[string]any` и `[]any` —
// канонические типизированные контейнеры payload-а, согласованные с
// контрактом [audit.MaskSecrets] в `shared/audit`. Все остальные типы
// (включая `map[string]string`, `[]string`, struct, указатели) копируются
// shallow — caller обязан строить Payload через `map[string]any`+`[]any`,
// иначе появляется potential shared-state между primary и secondary.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return clonePayload(x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = cloneValue(el)
		}
		return out
	default:
		return v
	}
}
