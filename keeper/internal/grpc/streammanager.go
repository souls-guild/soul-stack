package grpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// outboundBufferSize — длина per-stream outbound-канала (PM-decision 1
// M2.5: buffered=10 + drop+log при переполнении). Stale-stream не
// блокирует scenario-runner-а; back-pressure обратно в caller-а
// обрабатывается явно — caller получает [ErrOutboundQueueFull].
//
// Размер выбран эмпирически: при нормальном flow один Soul видит
// последовательно `ApplyRequest` (1 шт. на прогон), редкие
// `CancelApply` и `SeedRotationReply` — 10 элементов покрывают burst
// «оператор повторно нажал Cancel» без накопления mailbox-а.
const outboundBufferSize = 10

// Sentinel-ошибки [StreamManager] / [Outbound].
var (
	// ErrSoulNotConnected — нет активного EventStream-а на этом Keeper-е
	// для запрошенного SID. Caller (scenario-runner) обязан проверить
	// `souls.last_seen_at` и/или Redis heartbeat-кэш до Send-а — здесь
	// мы фиксируем факт «у конкретно этого Keeper-инстанса нет стрима»
	// (cluster-mode: другой Keeper может держать lease на тот же SID,
	// маршрутизация между Keeper-инстансами — отдельный slice).
	ErrSoulNotConnected = errors.New("grpc: no active EventStream for sid")

	// ErrOutboundQueueFull — per-stream outbound-канал переполнен. Soul
	// не успевает принимать (медленная сеть / зависший receive-loop на
	// клиенте). PM-decision 1: drop+log, caller получает sentinel и сам
	// решает (для ApplyDispatch — fail прогона; для CancelApply — retry
	// или skip; для SeedRotationReply — Soul retry-ует через свой
	// rotation-loop).
	ErrOutboundQueueFull = errors.New("grpc: outbound queue full for sid")
)

// streamEntry — per-stream state, хранящийся в [StreamManager].
//
// Лежит за указателем, чтобы Lookup мог вернуть его наружу и
// гарантировать, что concurrent Unregister не вырвет канал из-под
// активного caller-а (channel close — единственная race-free операция,
// см. [StreamManager.Unregister]).
//
// cancel — отмена per-stream ctx (см. [StreamManager.RegisterStream]). Нужна
// активному shedding-у (Watchman, soul-shedding S2): при устойчивой изоляции
// Keeper-инстанса [StreamManager.CloseAll] отменяет ctx каждого стрима →
// EventStream-handler выходит из receive-loop-а, делает штатный teardown
// (Unregister/lease-Release LIFO), gRPC шлёт Soul-у EOF, и Soul по
// reconnect-loop/failback-list уходит на живой Keeper. nil — стрим
// зарегистрирован без cancel (тесты/Outbound через [StreamManager.Register]):
// CloseAll такой стрим пропускает (закрытие outCh-канала всё равно произойдёт
// штатным Unregister-ом).
type streamEntry struct {
	sid     string
	outCh   chan *keeperv1.FromKeeper
	cancel  context.CancelFunc
	closeMu sync.Mutex
	closed  bool
}

// StreamManager — реестр активных EventStream-ов на текущем Keeper-инстансе.
//
// Ключ — SID (authoritative из mTLS peer-cert; см. [authenticatedSIDFrom]).
// Значение — outbound-channel, в который пишутся `FromKeeper`-сообщения;
// send-loop EventStream-handler-а вычитывает их и зовёт `stream.Send`.
//
// Cluster-mode: per-Keeper-инстанс реестр; routing между Keeper-инстансами
// (когда Soul держит стрим на Keeper-B, а Operator API вызывает SendApply
// на Keeper-A) — отдельный slice (post-M2.5, через Redis pub/sub).
type StreamManager struct {
	mu      sync.RWMutex
	entries map[string]*streamEntry
	logger  *slog.Logger
}

// NewStreamManager собирает пустой реестр.
func NewStreamManager(logger *slog.Logger) *StreamManager {
	return &StreamManager{
		entries: make(map[string]*streamEntry),
		logger:  logger,
	}
}

// Register регистрирует новый стрим для SID-а БЕЗ per-stream cancel-а и
// возвращает outbound-channel. Тонкая обёртка над [StreamManager.RegisterStream]
// для caller-ов, которым shedding не нужен (unit-тесты, ad-hoc регистрация в
// Outbound-тестах): такой стрим [StreamManager.CloseAll] не отменяет (нечего
// отменять), штатный teardown остаётся через Unregister.
func (m *StreamManager) Register(sid string) <-chan *keeperv1.FromKeeper {
	return m.RegisterStream(sid, nil)
}

// RegisterStream регистрирует новый стрим для SID-а с per-stream cancel-ом и
// возвращает outbound-channel, который handler передаёт send-loop-у. Если на
// тот же SID уже есть запись — старая закрывается (channel.close), новый стрим
// вытесняет старый.
//
// cancel — отмена производного от `stream.Context()` per-stream ctx; даёт
// активному shedding-у ([StreamManager.CloseAll], Watchman S2) точку
// принудительного закрытия стрима. nil допустим (см. [streamEntry.cancel] /
// [StreamManager.Register]).
//
// Вытеснение симметрично Redis SoulLease (см. [eventStreamHandler.acquireSoulLease]):
// если внутри одного Keeper-инстанса Soul переподключился (например,
// после клиент-side reconnect), новый стрим имеет приоритет — старый
// receive-loop в любом случае получит io.EOF/Canceled на следующем Recv.
// cancel вытесняемого стрима НЕ дёргается: его receive-loop уже выходит сам
// (eviction идёт по новому Register-у того же SID-а, т.е. Soul переподключился —
// старый стрим закрывается gRPC-уровнем), а лишняя отмена прошла бы по уже
// сменившемуся в map-е entry.
func (m *StreamManager) RegisterStream(sid string, cancel context.CancelFunc) <-chan *keeperv1.FromKeeper {
	m.mu.Lock()
	defer m.mu.Unlock()

	if old, ok := m.entries[sid]; ok {
		old.close()
		m.logger.Warn("streammanager: existing stream evicted by new Register",
			slog.String("sid", sid))
	}

	entry := &streamEntry{
		sid:    sid,
		outCh:  make(chan *keeperv1.FromKeeper, outboundBufferSize),
		cancel: cancel,
	}
	m.entries[sid] = entry
	return entry.outCh
}

// Unregister удаляет запись и закрывает outbound-channel. Idempotent:
// повторный вызов — no-op. Caller (handler defer) обязан вызвать после
// окончания receive-loop-а.
//
// Принимает SID и pointer на entry-владельца (через owner-handle получаем
// гарантию, что мы удаляем СВОЙ entry, а не вытеснителя из конкурентного
// Register).
func (m *StreamManager) Unregister(sid string, owner <-chan *keeperv1.FromKeeper) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[sid]
	if !ok {
		return
	}
	// Сравнение каналов — это сравнение указателей внутри chan-header-а;
	// если в map уже лежит более новый entry (нас вытеснил Register),
	// owner-канал не совпадает — не трогаем.
	if (<-chan *keeperv1.FromKeeper)(entry.outCh) != owner {
		return
	}
	entry.close()
	delete(m.entries, sid)
}

// lookup — read-lock получение entry. nil если стрима нет.
func (m *StreamManager) lookup(sid string) *streamEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.entries[sid]
}

// SIDs возвращает снимок SID-ов всех активных стримов на этом Keeper-инстансе.
// Снимок копируется под RLock — caller итерирует свободно, конкурентные
// Register/Unregister не держатся. Используется cluster-wide Sigil-re-broadcast-ом
// (S6c): по invalidate-сигналу нода раздаёт свежий active-набор каждому своему
// подключённому Soul-у. Порядок не гарантирован (map-итерация).
func (m *StreamManager) SIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.entries))
	for sid := range m.entries {
		out = append(out, sid)
	}
	return out
}

// CloseAll принудительно закрывает ВСЕ локальные стримы — отменяет per-stream
// ctx каждого зарегистрированного entry (soul-shedding S2, Watchman). Возвращает
// число стримов, у которых cancel был дёрнут (для лога/метрики caller-а).
//
// Отмена ctx будит receive-loop EventStream-handler-а (`ctx.Err() != nil` →
// return nil), и handler делает СВОЙ штатный teardown (Unregister →
// lease-Release LIFO). CloseAll сам outCh-каналы НЕ закрывает и записи из map-а
// НЕ удаляет — это сделает handler-овский Unregister, чтобы не разъехаться с
// его defer-цепочкой (двойное закрытие/удаление-под-ногами). Поэтому повторный
// CloseAll до того, как handler-ы реально вышли, снова дёрнет те же cancel-ы —
// это идемпотентно (context.CancelFunc безопасна к повторному вызову).
//
// Снимок cancel-ов снимается под RLock, сами cancel-ы вызываются вне lock-а:
// cancel дешёвый, но teardown handler-а (он сработает синхронно с отменой в
// другой горутине) может попытаться взять Lock на Unregister — держать write-lock
// тут было бы deadlock-prone, RLock + вызов снаружи исключает это.
func (m *StreamManager) CloseAll() int {
	m.mu.RLock()
	cancels := make([]context.CancelFunc, 0, len(m.entries))
	for _, e := range m.entries {
		if e.cancel != nil {
			cancels = append(cancels, e.cancel)
		}
	}
	m.mu.RUnlock()

	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// close — идемпотентное закрытие канала. Под closeMu, чтобы повторный
// Unregister/eviction не паниковал на double-close.
func (e *streamEntry) close() {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.outCh)
}

// send — non-blocking enqueue. true → принято, false → channel-buffer
// полон или закрыт (расцениваем оба как fail; closed-канал не должен
// существовать без Unregister-а, но защищаемся от race-окна).
func (e *streamEntry) send(msg *keeperv1.FromKeeper) bool {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return false
	}
	e.closeMu.Unlock()

	select {
	case e.outCh <- msg:
		return true
	default:
		return false
	}
}
