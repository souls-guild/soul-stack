// Package applybus — in-memory pub/sub шина apply-событий по apply_id.
//
// Назначение — связать publisher-ов (handler-ы EventStream-payload-ов
// TaskEvent / RunResult, в будущем — keeper-side scenario-runner) с
// subscriber-ами SSE-стрима `GET /mcp/events?apply_id=<ULID>` (M0.7.c).
//
// Контракт по PM-decision-ам M0.7.c:
//
//   - in-memory single-Keeper; cluster-wide pub/sub через Redis — отдельный
//     слой, активируется через [NewBusWithRedis] (M2.6, ADR-006(c));
//   - per-subscriber buffer 64 события: переполнение → drop oldest + warn;
//   - subscriber живёт до cancel-а его ctx (Unsubscribe идемпотентен);
//   - Publish — non-blocking: одна late-доставка не должна блокировать
//     publisher-а (EventStream-handler).
//
// Cluster-mode (M2.6, ADR-006(c)): при non-nil redis/kid Publish
// дополнительно публикует событие в шардированный Redis-канал
// `events:shard:<n>` (applyID → shard через [keeperredis.ApplyBusChannel])
// через [keeperredis.PublishApplyEvent]. Redis-bridge поднимается per-SHARD,
// а не per-applyID (S2 applybus-bottleneck, ADR-006(c) amendment): первый
// Subscribe любого applyID, отображённого в данный shard, поднимает одну
// подписку на shard-канал; последующие applyID того же shard-а её
// переиспользуют (refs считаются по shard-у). forward-loop фильтрует входящие
// события по `envelope.ApplyID` и раздаёт только в local-subscriber-ов
// соответствующего applyID. Self-filter по origin_kid отсекает эхо собственных
// публикаций (см. doc-comment `keeper/internal/redis/applybus.go`).
package applybus

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// EventKind — тип apply-события. Стабильные snake_case-имена, попадают в
// SSE-event-тип (`event: task.executed\n…`). Список фиксированный; новые
// kind-ы добавляются здесь и в pub/sub-publisher-ах.
type EventKind string

const (
	// KindApplyStarted — apply-прогон начат. Источник: keeper-side
	// scenario-runner (M2.x). В M0.7.c publisher отсутствует — оставлен в
	// контракте для forward-compat с SSE-клиентами.
	KindApplyStarted EventKind = "apply.started"

	// KindTaskExecuted — одна задача внутри прогона завершилась (любой
	// статус, см. TaskStatus в proto). Источник — events_taskevent.go.
	KindTaskExecuted EventKind = "task.executed"

	// KindApplyCompleted — прогон завершился успешно. Источник —
	// events_runresult.go при RUN_STATUS_SUCCESS.
	KindApplyCompleted EventKind = "apply.completed"

	// KindApplyFailed — прогон завершился ошибкой (FAILED / ERROR_LOCKED).
	// Источник — events_runresult.go.
	KindApplyFailed EventKind = "apply.failed"

	// KindApplyCancelled — прогон отменён (CANCELLED). Источник —
	// events_runresult.go.
	KindApplyCancelled EventKind = "apply.cancelled"

	// KindErrandCompleted / KindErrandFailed / KindErrandTimedOut /
	// KindErrandCancelled / KindErrandModuleNotAllowed — терминальные
	// статусы Errand-а (ADR-033). Источник — events_errand.go (handler
	// FromSoul.ErrandResult). Errand-события ходят по тому же shard-namespace
	// `events:shard:<n>`, что и apply-прогоны: errand_id отображается в shard
	// через [keeperredis.ApplyBusChannel] наравне с apply_id, forward-loop
	// фильтрует входящие по `ev.ApplyID` (см. doc-comment пакета).
	//
	// Семья отдельная от apply.*: SSE-клиенты, фильтрующие по `kind`,
	// смогут различать Errand-события от apply-прогонов.
	KindErrandCompleted        EventKind = "errand.completed"
	KindErrandFailed           EventKind = "errand.failed"
	KindErrandTimedOut         EventKind = "errand.timed_out"
	KindErrandCancelled        EventKind = "errand.cancelled"
	KindErrandModuleNotAllowed EventKind = "errand.module_not_allowed"
)

// Event — одно apply-событие, доставляемое subscriber-у.
//
// Payload — произвольный typed-payload, который publisher сериализует под
// конкретный SSE-event. SSE-handler делает json.Marshal(Payload) в `data:`-блок.
// Структура payload-ов фиксируется в docs/keeper/mcp-tools.md → § SSE (отдельный
// slice документа).
type Event struct {
	ApplyID string
	Kind    EventKind
	At      time.Time
	Payload any
}

// SubscriberBufferSize — буфер per-subscriber канала. Значение «магического»
// порядка: 64 события покрывают типичный apply-прогон (10–30 задач + старт +
// финал) с запасом; при переполнении срабатывает drop-oldest (см.
// [EventBus.Publish]). Выносить в конфиг смысла нет — это внутренний
// flow-control, симметрично StreamManager (PM-decision M2.5 buffered=10).
const SubscriberBufferSize = 64

// clusterPublishTimeout — deadline на сетевой Redis PUBLISH в cluster-mode
// (см. [EventBus.publishToCluster]): если Redis недоступен, publisher не
// блокируется дольше этого; ошибка → warn, не возврат.
const clusterPublishTimeout = time.Second

// subscriber — один подписчик на конкретный apply_id.
//
// ch НИКОГДА не закрывается (вариант A done-канала): закрытие канала, в
// который Publish пишет вне lock-а, неустранимо гонит с доставкой ("send on
// closed channel"). Вместо этого отписка закрывает done; [EventBus.deliver]
// в select-е на done тихо прекращает доставку. Потребитель ориентируется на
// свой ctx (см. [EventBus.Subscribe]), не на close(ch).
type subscriber struct {
	ch   chan Event
	done chan struct{}
	// heldBridge — true, если этот subscriber инкрементировал refs
	// Redis-bridge на своём SHARD-е (Subscribe с wantBridge=true в
	// cluster-mode). unsubscribe декрементит refs шарда только для таких —
	// иначе local-only subscriber (wantBridge=false) ошибочно уронил бы
	// bridge, поднятый соседним subscriber-ом того же shard-а.
	heldBridge bool
}

// clusterBridge — handle на одну Redis-подписку shard-канала
// `events:shard:<n>`, активна пока на shard-е есть хотя бы один
// local-subscriber с heldBridge.
//
// refs — счётчик held-subscriber-ов ПО ШАРДУ (несколько applyID шарят одну
// подписку). На refs=0 bridge закрывается (см. [EventBus.unsubscribe]).
// cancel — отмена ctx подписки; sub — сам
// [keeperredis.ApplyEventSubscription] (закрытие через sub.Close).
type clusterBridge struct {
	refs   int
	cancel context.CancelFunc
	sub    *keeperredis.ApplyEventSubscription
}

// EventBus — pub/sub шина apply-событий. Потокобезопасна.
//
// Внутри: map[apply_id][]*subscriber под RWMutex. Subscribe держит
// read-lock-ом publishers недолго (только snapshot среза subscriber-ов под
// конкретный apply_id), но саму доставку делает уже вне lock-а — иначе один
// медленный subscriber застопорил бы публикацию для всех остальных тем же
// apply_id.
//
// При non-nil redis/kid (cluster-mode, M2.6) bus также держит per-SHARD
// Redis-bridge через [clusterBridge], форвардит cross-Keeper события в
// local subscribers (фильтр по applyID) и публикует local events в Redis.
type EventBus struct {
	mu sync.RWMutex
	// subs — local-subscriber-ы по applyID (доставка таргетирована).
	subs map[string][]*subscriber
	// bridges — Redis-подписки по индексу shard-канала (несколько applyID
	// шарят одну подписку). Ключ — [keeperredis.ApplyBusShardIndex](applyID).
	bridges map[uint32]*clusterBridge
	redis   *keeperredis.Client
	kid     string
	logger  *slog.Logger
}

// NewBus собирает single-Keeper шину (без Redis-bridge). logger обязателен —
// drop-oldest-warning и late-publish-сообщения уходят в slog.
func NewBus(logger *slog.Logger) *EventBus {
	return NewBusWithRedis(logger, nil, "")
}

// NewBusWithRedis собирает шину с опциональным cluster-bridge через Redis
// (ADR-006(c)). При redis=nil или kid="" cluster-mode выключен —
// поведение идентично [NewBus] (single-Keeper).
//
// kid обязателен для self-filter-а: cluster-bridge отбрасывает события с
// origin_kid == собственный KID, иначе local-Publish + Redis-echo приведёт
// к двойной доставке SSE-клиентам.
func NewBusWithRedis(logger *slog.Logger, redis *keeperredis.Client, kid string) *EventBus {
	if logger == nil {
		logger = slog.Default()
	}
	b := &EventBus{
		subs:   make(map[string][]*subscriber),
		logger: logger,
	}
	if redis != nil && kid != "" {
		b.redis = redis
		b.kid = kid
		b.bridges = make(map[uint32]*clusterBridge)
	}
	return b
}

// clusterEnabled — true, если шина настроена для cluster-mode (есть
// redis-клиент и KID).
func (b *EventBus) clusterEnabled() bool {
	return b.redis != nil && b.kid != ""
}

// Subscribe возвращает канал, в который попадут все события для applyID,
// опубликованные ПОСЛЕ возврата из Subscribe. Late-subscribe не получит
// уже отданные publisher-у события (это pub/sub bus, не лог).
//
// Подписка завершается по ctx.Done() (SSE-клиент отключился или
// handler-context выкатился) — внутренняя goroutine делает unsubscribe.
// Канал НЕ закрывается: потребитель ориентируется на свой ctx, а не на
// close(ch) (см. doc-comment типа subscriber). Caller не должен явно
// вызывать Unsubscribe.
//
// В cluster-mode на первый Subscribe(applyID), отображённый в данный shard,
// поднимается Redis-bridge на канал `events:shard:<n>`; на last-Unsubscribe
// последнего held-applyID того же shard-а — bridge закрывается. Ready
// Redis-подписки дожидается синхронно до возврата (чтобы исключить race
// «Subscribe вернулся → Publish на другом Keeper-е → подписка ещё не
// зарегистрирована в Redis»).
//
// Эквивалентно SubscribeWithBridge(ctx, applyID, true) — поведение
// сохранено для back-compat всех existing caller-ов.
func (b *EventBus) Subscribe(ctx context.Context, applyID string) <-chan Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

// SubscribeWithBridge — вариант [Subscribe] с явным управлением Redis-bridge.
//
// wantBridge=false пропускает [EventBus.ensureClusterBridgeLocked]: подписка
// получает только local-доставку (через [EventBus.Publish] того же инстанса),
// но НЕ cross-Keeper события из Redis. Остальной lifecycle (регистрация
// subscriber-а, refs/unsubscribe по ctx, не-закрываемый ch) идентичен
// [Subscribe] — один code-path.
//
// Применение (S1, applybus-bottleneck): caller, заранее знающий, что событие
// придёт только от local publisher-а того же инстанса (lease-holder целевого
// SID == self-KID), просит wantBridge=false — это снимает per-applyID
// Redis-Subscribe и устраняет maxclients-cliff на больших флотах. Если holder
// неизвестен/может смениться — caller обязан запросить wantBridge=true
// (консервативно), иначе cross-Keeper событие до подписки не дойдёт.
//
// В single-keeper-режиме (cluster выключен) wantBridge игнорируется — bridge
// и так не поднимается.
func (b *EventBus) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan Event {
	if ctx == nil {
		ch := make(chan Event)
		close(ch)
		return ch
	}
	if applyID == "" {
		// Защита от случайных Subscribe("") — это явный bug caller-а, ничего
		// полезного из такого канала не придёт.
		ch := make(chan Event)
		close(ch)
		return ch
	}

	sub := &subscriber{
		ch:   make(chan Event, SubscriberBufferSize),
		done: make(chan struct{}),
	}

	b.mu.Lock()
	b.subs[applyID] = append(b.subs[applyID], sub)
	var bridgeReady <-chan struct{}
	if wantBridge && b.clusterEnabled() {
		// refs инкрементится только при реальном поднятии/переиспользовании
		// bridge-а; heldBridge гарантирует симметричный decrement в
		// unsubscribe (см. doc-comment subscriber.heldBridge).
		sub.heldBridge = true
		bridgeReady = b.ensureClusterBridgeLocked(applyID)
	}
	b.mu.Unlock()

	if bridgeReady != nil {
		// Дожидаемся Ready без holding lock-а. Если bridge упал до Ready —
		// просто игнорируем (local-доставка продолжит работать,
		// cross-Keeper события не дойдут до этого subscriber-а).
		select {
		case <-bridgeReady:
		case <-ctx.Done():
		}
	}

	go func() {
		<-ctx.Done()
		b.unsubscribe(applyID, sub)
	}()

	return sub.ch
}

// ensureClusterBridgeLocked поднимает (или инкрементирует refs) Redis-bridge
// на SHARD, в который отображается applyID. Вызывается под write-lock-ом b.mu.
//
// Возвращает chan, на котором subscribe-loop сигналит Ready. nil — если
// cluster-mode выключен или bridge на этот shard уже был создан (refs++).
func (b *EventBus) ensureClusterBridgeLocked(applyID string) <-chan struct{} {
	if !b.clusterEnabled() {
		return nil
	}
	shard := keeperredis.ApplyBusShardIndex(applyID)
	if br, ok := b.bridges[shard]; ok {
		br.refs++
		return nil
	}

	// Background-ctx, потому что bridge должен жить до явного refs=0,
	// независимо от ctx первого Subscribe-а. applyID передаём в
	// SubscribeApplyEvent лишь как shard-селектор/лог-метку — подписка идёт
	// на shard-канал, на него льются все applyID того же shard-а.
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := keeperredis.SubscribeApplyEvent(ctx, b.redis, applyID, b.kid, b.logger)
	if err != nil {
		cancel()
		b.logger.Warn("applybus: cluster-bridge subscribe failed",
			slog.Uint64("shard", uint64(shard)),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
		return nil
	}
	br := &clusterBridge{refs: 1, cancel: cancel, sub: sub}
	b.bridges[shard] = br

	ready := make(chan struct{})
	go func() {
		// Ждём готовности Redis-подписки и сигналим caller-у.
		readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
		defer readyCancel()
		if err := sub.Ready(readyCtx); err != nil {
			b.logger.Warn("applybus: cluster-bridge Ready failed",
				slog.Uint64("shard", uint64(shard)),
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		close(ready)

		// Forward-loop: Redis-event → local subscribers. applyID берём из
		// самого события (на shard-канал приходят разные applyID), чужие
		// отсеиваются отсутствием local-subscriber-ов в [deliverFromCluster].
		// Завершается при закрытии sub.Channel() (Close через unsubscribe).
		for ev := range sub.Channel() {
			b.deliverFromCluster(ev)
		}
	}()
	return ready
}

// deliverFromCluster доставляет cross-Keeper событие local-subscriber-ам
// именно того applyID, что несёт событие (`ev.ApplyID`). Это и есть shard-
// фильтр: на один shard-канал льются события многих applyID, но каждое уходит
// только в b.subs[ev.ApplyID]; если local-subscriber-ов на этот applyID нет
// (чужое событие коллидирующего applyID), снимок пуст → no-op.
//
// Origin-filter уже сделан в SubscribeApplyEvent (см. self-filter в
// `keeper/internal/redis/applybus.go`); здесь только конвертация
// json.RawMessage payload → any (json.RawMessage сам реализует
// json.Marshaler, поэтому SSE-handler корректно пере-сериализует его в
// frame без двойной перекодировки).
func (b *EventBus) deliverFromCluster(ev *keeperredis.ApplyEvent) {
	b.mu.RLock()
	subs := b.subs[ev.ApplyID]
	if len(subs) == 0 {
		b.mu.RUnlock()
		return
	}
	snapshot := make([]*subscriber, len(subs))
	copy(snapshot, subs)
	b.mu.RUnlock()

	out := Event{
		ApplyID: ev.ApplyID,
		Kind:    EventKind(ev.Kind),
		At:      ev.At,
		Payload: ev.Payload,
	}
	for _, s := range snapshot {
		b.deliver(out, s)
	}
}

// Publish доставляет ev всем активным subscriber-ам на ev.ApplyID. Pure
// non-blocking: если канал subscriber-а полон, мы drop-аем самое старое
// событие и пишем warn. Это сохраняет проперть «publisher никогда не
// блокируется» в обмен на возможность пропустить событие у slow client-а.
//
// Late subscriber — событие, отправленное до Subscribe, потеряно (in-memory
// bus). Это сознательное упрощение MVP: SSE-клиент обязан подписаться ДО
// async-tool-вызова (порядок «subscribe → tools/call → ждать SSE-events»).
//
// В cluster-mode (см. [NewBusWithRedis]) после local-доставки событие
// дополнительно публикуется в shard-канал `events:shard:<n>` (applyID →
// shard) через [keeperredis.PublishApplyEvent]. Subscriber-ы на других
// Keeper-инстансах, подписанные на тот же shard, получат событие через свой
// cluster-bridge и отфильтруют по applyID. Ошибки Redis-publish-а
// логируются как warn — local-доставка уже произошла, publisher всё равно
// не блокируется.
func (b *EventBus) Publish(ev Event) {
	if ev.ApplyID == "" {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}

	// Снимаем snapshot под read-lock-ом, дальше пишем уже без блокировки
	// шины — медленный subscriber не мешает остальным.
	b.mu.RLock()
	subs := b.subs[ev.ApplyID]
	snapshot := make([]*subscriber, len(subs))
	copy(snapshot, subs)
	b.mu.RUnlock()

	for _, s := range snapshot {
		b.deliver(ev, s)
	}

	if b.clusterEnabled() {
		b.publishToCluster(ev)
	}
}

// publishToCluster сериализует payload и публикует в Redis. Pure
// best-effort: ошибки логируются и игнорируются (publisher продолжает
// работать, local-доставка уже сделана).
func (b *EventBus) publishToCluster(ev Event) {
	var payload json.RawMessage
	if ev.Payload != nil {
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			b.logger.Warn("applybus: cluster-publish payload marshal failed",
				slog.String("apply_id", ev.ApplyID),
				slog.String("kind", string(ev.Kind)),
				slog.Any("error", err),
			)
			return
		}
		payload = raw
	}
	// Deadline на сетевой PUBLISH: если Redis недоступен, не блокируем
	// publisher-а долго. Ошибка → warn, не возврат.
	ctx, cancel := context.WithTimeout(context.Background(), clusterPublishTimeout)
	defer cancel()
	if _, err := keeperredis.PublishApplyEvent(ctx, b.redis, ev.ApplyID, b.kid, string(ev.Kind), ev.At, payload); err != nil {
		b.logger.Warn("applybus: cluster-publish failed",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
			slog.Any("error", err),
		)
	}
}

// deliver кладёт событие в канал subscriber-а. При full-канале drop-аем
// самое старое (вычитываем один элемент, чтобы освободить слот) и пишем
// предупреждение в лог. Дроп-oldest предпочтительнее дроп-newest, потому
// что для SSE-клиента самое свежее состояние полезнее старого
// «task_idx=0 OK».
func (b *EventBus) deliver(ev Event, s *subscriber) {
	select {
	case s.ch <- ev:
		return
	case <-s.done:
		// Subscriber отписан — доставка тихо прекращается (ch не закрыт,
		// паники нет).
		return
	default:
	}
	// Полный канал — пытаемся освободить слот. Read из не-закрываемого
	// канала безопасен и не гонит с отпиской.
	select {
	case <-s.ch:
		b.logger.Warn("applybus: subscriber buffer full — dropped oldest event",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
			slog.Int("buffer", SubscriberBufferSize),
		)
	default:
		// Канал уже опустошён конкурентным reader-ом между двумя select-ами
		// — просто пишем новое событие ниже.
	}
	select {
	case s.ch <- ev:
	case <-s.done:
		return
	default:
		// Маловероятно (subscriber полностью deadlocked), но не блокируемся:
		// гарантия publisher-non-block важнее одного события.
		b.logger.Warn("applybus: subscriber still full after drop — event lost",
			slog.String("apply_id", ev.ApplyID),
			slog.String("kind", string(ev.Kind)),
		)
	}
}

// unsubscribe удаляет subscriber-а из map-ы и закрывает его done-канал.
// Вызов идемпотентен: повторный вызов на уже удалённого subscriber-а — no-op
// (ранний возврат по idx<0, done закрывается ровно один раз).
//
// s.ch НЕ закрывается никогда. Закрытие канала, в который Publish пишет вне
// lock-а, неустранимо гонит с доставкой. Вместо этого закрываем done; deliver
// в select-е на done тихо прекращает доставку (см. [EventBus.deliver]).
// Потребитель завершается по своему ctx, а не по close(ch).
//
// При last-Unsubscribe последнего held-applyID того же shard-а (cluster-mode)
// закрывается соответствующий Redis-bridge — bridge.refs декрементится, на нуле
// bridge.cancel + sub.Close тормозят forward-loop. Закрытие bridge-а делаем
// после релиза lock-а.
func (b *EventBus) unsubscribe(applyID string, s *subscriber) {
	b.mu.Lock()
	subs := b.subs[applyID]
	idx := -1
	for i, x := range subs {
		if x == s {
			idx = i
			break
		}
	}
	if idx < 0 {
		b.mu.Unlock()
		return
	}
	// Удаляем без сохранения порядка — O(1) swap-and-truncate.
	subs[idx] = subs[len(subs)-1]
	subs[len(subs)-1] = nil
	subs = subs[:len(subs)-1]
	if len(subs) == 0 {
		delete(b.subs, applyID)
	} else {
		b.subs[applyID] = subs
	}

	var bridgeToClose *clusterBridge
	if b.clusterEnabled() && s.heldBridge {
		// Декрементим refs ШАРДА только если этот subscriber держал bridge-ref
		// (wantBridge=true). local-only subscriber (wantBridge=false) refs
		// не трогает — иначе уронил бы bridge соседа того же shard-а.
		//
		// refs >= 1 удерживает именно ЭТОТ инстанс bridge от схлопывания: пока
		// хоть один held-subscriber жив, bridge.refs > 0 и shard-подписка живёт.
		// Декремент адресован по shardIndex(applyID) под b.mu — он бьёт строго
		// по bridge СВОЕГО shard-а; bridge чужого shard-а (другой ключ в map-е)
		// не задевается, конкурентный unsubscribe чужого инстанса bridge тоже
		// сериализован тем же b.mu, поэтому refs не уходит в минус.
		shard := keeperredis.ApplyBusShardIndex(applyID)
		if br, ok := b.bridges[shard]; ok {
			br.refs--
			if br.refs <= 0 {
				delete(b.bridges, shard)
				bridgeToClose = br
			}
		}
	}
	b.mu.Unlock()

	close(s.done)

	if bridgeToClose != nil {
		bridgeToClose.cancel()
		_ = bridgeToClose.sub.Close()
	}
}

// Subscribers возвращает количество активных подписчиков на applyID. Только
// для тестов / диагностики.
func (b *EventBus) Subscribers(applyID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[applyID])
}
