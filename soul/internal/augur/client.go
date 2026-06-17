// Package augur — Soul-side клиент брокера Augur (ADR-025, docs/keeper/augur.md).
//
// Augur даёт Soul-у живой (во время apply) доступ к внешним системам через
// Keeper: модуль core.augur.fetch шлёт AugurRequest в EventStream и ждёт
// коррелированный AugurReply. Транспорт — only-add сообщения в существующем
// EventStream-е (FromSoul.augur_request / FromKeeper.augur_reply), нового RPC
// нет (ADR-012(c)).
//
// Клиент живёт ровно одну EventStream-сессию: pending-map коррелирует
// in-flight-запросы по request_id, recv-loop сессии доставляет AugurReply.
// При разрыве/закрытии сессии все ожидающие отменяются (Close) — request_id
// уникален лишь per-stream (§5.1 augur.md), поэтому переживать reconnect ему
// незачем.
//
// MVP-1 (delegate=false): Soul получает значение inline через Keeper
// (AugurReply.inline_data). Делегация (delegate=true, scoped_*) — MVP-2, здесь
// не обрабатывается.
package augur

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/oklog/ulid/v2"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ErrClientClosed — клиент закрыт (сессия EventStream завершилась) до того, как
// пришёл AugurReply. Возвращается из Fetch ожидающим запросам при Close.
var ErrClientClosed = errors.New("augur: клиент закрыт (EventStream-сессия завершена)")

// ErrDenied — Augur отказал в доступе (AugurStatus DENIED либо защитная
// трактовка UNSPECIFIED как deny, §5.1 augur.md). Несёт причину от Keeper-а
// без секретного материала.
var ErrDenied = errors.New("augur: доступ запрещён")

// ErrRemote — сбой исполнения на стороне Keeper-а/Omen (AugurStatus ERROR).
var ErrRemote = errors.New("augur: ошибка исполнения на Keeper-е")

// requestSender — узкая поверхность EventStream-сессии, нужная клиенту: только
// отправка FromSoul. Выделена ради тестируемости без живого gRPC и чтобы клиент
// не зависел от soul/internal/grpc (тот зависит от runtime — циклов избегаем).
//
// Не concurrent-safe у реальной сессии (один writer на bidi-stream); клиент
// сериализует Send под sendMu.
type requestSender interface {
	SendFromSoul(*keeperv1.FromSoul) error
}

// Client — Augur-клиент одной EventStream-сессии.
//
// Один writer (sendMu сериализует Send в stream — bidi-stream не допускает
// concurrent Send). Доставку AugurReply делает recv-loop сессии через Deliver:
// он НЕ блокируется (буферизованный канал на 1 + неблокирующая запись).
type Client struct {
	sender requestSender
	// entropy — монотонный источник для ULID request_id. Под sendMu (генерация
	// идёт в момент отправки) — отдельного мьютекса не нужно.
	entropy *ulid.MonotonicEntropy

	sendMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan *keeperv1.AugurReply
	closed  bool
}

// NewClient собирает клиент поверх EventStream-сессии. Источник энтропии для
// request_id монотонный (lexically-sortable, без коллизий в пределах сессии).
func NewClient(sender requestSender) *Client {
	return &Client{
		sender:  sender,
		entropy: ulid.Monotonic(rand.Reader, 0),
		pending: make(map[string]chan *keeperv1.AugurReply),
	}
}

// Fetch шлёт AugurRequest и блокируется до коррелированного AugurReply, отмены
// ctx или закрытия клиента. Возвращает inline_data (delegate=false, §5.3) при
// OK; ErrDenied/ErrRemote/ErrClientClosed иначе.
//
// request_id генерируется здесь (Soul-side ULID, уникален per-stream, §5.1).
// pending-канал регистрируется ДО Send — иначе быстрый AugurReply мог бы прийти
// в recv-loop раньше регистрации и потеряться.
func (c *Client) Fetch(ctx context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error) {
	reqID, replyCh, err := c.register()
	if err != nil {
		return nil, err
	}
	// Снимаем регистрацию при любом исходе — таймаут/отмена/доставка. Без этого
	// pending-map протекал бы на отменённых запросах.
	defer c.discard(reqID)

	req := &keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_AugurRequest{
			AugurRequest: &keeperv1.AugurRequest{
				RequestId: reqID,
				ApplyId:   applyID,
				OmenName:  omen,
				Query:     query,
			},
		},
	}
	if err := c.send(req); err != nil {
		return nil, fmt.Errorf("augur: отправка запроса: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply, ok := <-replyCh:
		if !ok {
			// Канал закрыт из Close — сессия порвалась, ответа не будет.
			return nil, ErrClientClosed
		}
		return classify(reply)
	}
}

// Deliver вызывается recv-loop-ом сессии при FromKeeper.augur_reply. НЕ
// блокируется: находит pending-канал по request_id и пишет в него неблокирующе
// (канал буферизован на 1, единственный потребитель Fetch уже ждёт либо ушёл по
// таймауту — во втором случае default отбрасывает поздний ответ). Возвращает
// true, если ответ был кому доставлен (для диагностики «осиротевший reply»).
func (c *Client) Deliver(reply *keeperv1.AugurReply) bool {
	if reply == nil {
		return false
	}
	c.mu.Lock()
	ch, ok := c.pending[reply.GetRequestId()]
	c.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- reply:
		return true
	default:
		// Потребитель уже ушёл (таймаут/cancel) — поздний ответ отбрасываем,
		// recv-loop не блокируется.
		return false
	}
}

// Close закрывает клиент: будущие Fetch отвергаются, все ожидающие получают
// ErrClientClosed (закрытие их каналов). Вызывается при завершении сессии
// EventStream. Идемпотентен.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// register генерирует request_id и регистрирует pending-канал. Канал
// буферизован на 1 — Deliver пишет неблокирующе даже если Fetch уже на грани
// select. Ошибка — только если клиент закрыт.
func (c *Client) register() (string, <-chan *keeperv1.AugurReply, error) {
	ch := make(chan *keeperv1.AugurReply, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", nil, ErrClientClosed
	}
	// ULID под mu (entropy мутируется монотонным генератором). Коллизия в
	// пределах сессии исключена монотонностью; перегенерация на случай
	// маловероятного дубля в map — defensive.
	var id string
	for {
		id = ulid.MustNew(ulid.Now(), c.entropy).String()
		if _, dup := c.pending[id]; !dup {
			break
		}
	}
	c.pending[id] = ch
	return id, ch, nil
}

// discard снимает pending-канал (Fetch завершился по таймауту/cancel/доставке).
// Безопасен при уже закрытом клиенте (Close мог удалить запись).
func (c *Client) discard(reqID string) {
	c.mu.Lock()
	delete(c.pending, reqID)
	c.mu.Unlock()
}

// send сериализует отправку FromSoul (bidi-stream — один writer).
func (c *Client) send(msg *keeperv1.FromSoul) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.sender.SendFromSoul(msg)
}

// classify интерпретирует AugurReply. UNSPECIFIED трактуется как DENIED
// (default-deny, §5.1 augur.md): отсутствие явного OK — запрет. delegate=true
// результаты (scoped_*) — MVP-2; в MVP-1 при OK ожидается inline_data.
func classify(reply *keeperv1.AugurReply) (*keeperv1.AugurReply, error) {
	switch reply.GetStatus() {
	case keeperv1.AugurStatus_AUGUR_STATUS_OK:
		if reply.GetInlineData() == nil {
			// OK без inline_data в MVP-1 — это либо delegate=true (не поддержан
			// здесь), либо рассогласование с Keeper-ом. Не молчим, явная ошибка.
			return nil, fmt.Errorf("augur: OK без inline_data (delegate=true не поддержан в MVP-1)")
		}
		return reply, nil
	case keeperv1.AugurStatus_AUGUR_STATUS_DENIED:
		return nil, wrapReason(ErrDenied, reply.GetError())
	case keeperv1.AugurStatus_AUGUR_STATUS_ERROR:
		return nil, wrapReason(ErrRemote, reply.GetError())
	default:
		// UNSPECIFIED и любой неизвестный статус — deny (защита).
		return nil, wrapReason(ErrDenied, reply.GetError())
	}
}

// wrapReason добавляет причину Keeper-а к sentinel-ошибке, если она есть.
// Причина приходит от Keeper-а без секрета (§8 augur.md: значения/токены в
// диагностику не пишутся).
func wrapReason(base error, reason string) error {
	if reason == "" {
		return base
	}
	return fmt.Errorf("%w: %s", base, reason)
}
