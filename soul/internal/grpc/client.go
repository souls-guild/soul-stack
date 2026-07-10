// Package grpc — Soul-side gRPC-клиент Keeper-а.
//
// Состав:
//   - [Client]: dial-цикл по списку endpoint-ов с приоритетами и spray-shuffle
//     ([keeper.endpoints] из soul.yml). Один Client → одна попытка установить
//     долгоживущий EventStream-стрим (ADR-002, ADR-012(a)).
//   - [StreamSession]: handshake (Hello/HelloReply) + send/recv-loop поверх
//     уже установленного bidi-стрима. Реализует runtime.EventSink.
//
// Reconnect — задача caller-а (`soul run`-loop в cmd/soul): при ошибке Dial
// или Recv-loop он повторяет с backoff-ом. Это упрощает тестирование (каждый
// шаг проверяется отдельно) и не размывает ответственность.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tlsx"
)

// Endpoint — описание Keeper-а из soul.yml. Addr — EventStream-адрес
// (`host:event_stream_port`, источник — SoulKeeperEndpoint.EventStreamAddr()).
// Приоритет — нативный порядок failover ([docs/soul/connection.md]): сначала
// пробуем priority 1, затем 2, внутри одного приоритета — случайный shuffle
// (spray).
type Endpoint struct {
	Addr     string
	Priority int
}

// ClientConfig — параметры Client.
type ClientConfig struct {
	// Endpoints — список Keeper-инстансов. Не пустой.
	Endpoints []Endpoint
	// SeedCert / SeedKey — клиентский SoulSeed (PEM-пути на диске).
	SeedCert string
	SeedKey  string
	// CAPath — CA-цепочка для верификации серверного cert-а Keeper-а.
	CAPath string
	// HandshakeTimeout — окно одного dial + Hello/HelloReply.
	HandshakeTimeout time.Duration
	// SoulVersion — кладётся в Hello.soul_version для аудита.
	SoulVersion string
	// SID — кладётся в Hello.sid_echo (authoritative — mTLS peer cert).
	SID string
	// MaxRecvMsgSize — потолок размера входящего FromKeeper в байтах (прежде
	// всего ApplyRequest с пачкой RenderedTask). Применяется как
	// grpc.MaxCallRecvMsgSize, заменяя малый gRPC-дефолт (4 MiB). 0 →
	// [config.DefaultMaxApplySizeMB] (8 MiB). Источник — soul.yml
	// `keeper.max_apply_size_mb` (config.SoulKeeper.ResolvedMaxApplySize).
	MaxRecvMsgSize int
	// MaxAttempts — число попыток dialOne на ОДИН endpoint при retriable-ошибке
	// (Unavailable/DeadlineExceeded/Internal/… см. isRetriablePerEndpoint),
	// прежде чем spray-ить к следующему endpoint. Источник — soul.yml
	// `keeper.retry.max_attempts`. 0 → [defaultMaxAttempts] (резолв в NewClient).
	// Внешний reconnectLoop остаётся отдельным уровнем повтора между ПОЛНЫМИ
	// проходами по fallback-list-у (ADR-002).
	MaxAttempts int
	// InterAttemptDelay — пауза между попытками к одному endpoint-у. ПЛОСКАЯ
	// (без экспоненциального роста — рост остаётся внешнему reconnectLoop).
	// Источник — `keeper.retry.backoff.initial` (реюз, без новых конфиг-ключей).
	// restart-required: значение фиксируется при сборке Client и НЕ перечитывается
	// при hot-reload (в отличие от reconnect-backoff, который reconnectLoop берёт
	// из store per-iteration) — смена `backoff.initial` для inter-attempt delay
	// требует рестарта soul.
	InterAttemptDelay time.Duration
	// InterAttemptJitter — добавлять ли ±25% jitter к InterAttemptDelay.
	// Источник — `keeper.retry.backoff.jitter`.
	InterAttemptJitter bool
}

// defaultMaxAttempts — число попыток dialOne на endpoint при опущенном/нулевом
// keeper.retry.max_attempts. Консервативное значение: одна повторная попытка
// после первого retriable-сбоя, дальше spray + внешний reconnectLoop.
const defaultMaxAttempts = 2

// Client — менеджер EventStream-сессий.
type Client struct {
	cfg    ClientConfig
	logger *slog.Logger
}

// NewClient собирает Client и валидирует обязательные поля.
func NewClient(cfg ClientConfig, logger *slog.Logger) (*Client, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("grpc: client: endpoints is empty")
	}
	if cfg.SeedCert == "" || cfg.SeedKey == "" || cfg.CAPath == "" {
		return nil, errors.New("grpc: client: SoulSeed (cert/key/ca) is incomplete — run `soul init` first")
	}
	if cfg.SID == "" {
		return nil, errors.New("grpc: client: SID is empty")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.MaxRecvMsgSize <= 0 {
		cfg.MaxRecvMsgSize = config.DefaultMaxApplySizeMB * 1024 * 1024
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{cfg: cfg, logger: logger}, nil
}

// Dial устанавливает gRPC-соединение и открывает EventStream + handshake.
// Возвращает StreamSession готовый к send/recv. На каждый endpoint — одна
// попытка; первый успех — используется.
//
// При полном фейле возвращает агрегированную ошибку с подробностями каждого
// endpoint-а — caller (run-loop) делает backoff и повторяет.
func (c *Client) Dial(ctx context.Context) (*StreamSession, error) {
	return c.DialPriority(ctx, 0)
}

// DialPriority — попытка установить соединение, ограничив поиск endpoint-ами
// с приоритетом строго меньше maxPriority. maxPriority=0 трактуется как
// «без ограничения» (эквивалент Dial). Используется failback-loop-ом
// для попытки вернуться на более предпочтительный приоритет ([docs/soul/connection.md
// → Failback]).
//
// Возвращает session и фактический priority выбранного endpoint-а через
// StreamSession.Priority(); при maxPriority>0 успех = priority < maxPriority,
// иначе error.
func (c *Client) DialPriority(ctx context.Context, maxPriority int) (*StreamSession, error) {
	tlsCfg, err := tlsx.LoadClientTLS(tlsx.ClientConfig{
		CertPath: c.cfg.SeedCert,
		KeyPath:  c.cfg.SeedKey,
		CAPath:   c.cfg.CAPath,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc: client: load mTLS: %w", err)
	}

	// gRPC keepalive: пингуем сервер каждые 30s после 10s idle, держим
	// connection alive если канал не используется (PermitWithoutStream).
	// Это покрывает ADR-012 «никакого app-level heartbeat» — gRPC сам
	// детектит мёртвое соединение и закрывает stream.
	kp := keepalive.ClientParameters{
		Time:                30 * time.Second,
		Timeout:             10 * time.Second,
		PermitWithoutStream: true,
	}

	var dialErrs []string
	// allLeaseHeld — все упавшие endpoint-ы вернули AlreadyExists (SID-lease ещё
	// держит живой/не-истёкший holder, keeper/internal/grpc/eventstream.go).
	// Спрашиваем итог только когда хоть один endpoint реально пробовался: при
	// пустом dialErrs (нет подходящих endpoint-ов) флаг остаётся ложным.
	allLeaseHeld := true
	tried := false
	for _, ep := range orderedEndpoints(c.cfg.Endpoints) {
		if maxPriority > 0 && normalizedPriority(ep.Priority) >= maxPriority {
			continue
		}
		cfgForAddr := tlsCfg.Clone()
		if h, ok := hostFromAddr(ep.Addr); ok {
			cfgForAddr.ServerName = h
		}
		creds := credentials.NewTLS(cfgForAddr)

		tried = true
		// Per-endpoint retry (keeper.retry.max_attempts): повторяем dialOne к ОДНОМУ
		// endpoint-у при retriable-ошибке (transport-flake), прежде чем spray-ить к
		// следующему. Non-retriable (lease-held / auth / invalid) — break сразу:
		// повтор к тому же endpoint-у бессмыслен, нужен другой. Это уровень между
		// одиночным dialOne и внешним reconnectLoop (ADR-002, docs/soul/connection.md).
		var err error
		var sess *StreamSession
		for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
			sess, err = c.dialOne(ctx, ep.Addr, creds, kp)
			if err == nil {
				sess.priority = normalizedPriority(ep.Priority)
				c.logger.Info("eventstream: connected",
					slog.String("addr", ep.Addr),
					slog.Int("priority", sess.priority),
					slog.String("kid", sess.KID()),
					slog.String("session_id", sess.SessionID()),
				)
				return sess, nil
			}
			if !isRetriablePerEndpoint(err) {
				// ★ Регресс-guard (7af8e95): lease-held (AlreadyExists) — non-retriable,
				// поэтому на каждый lease-held endpoint РОВНО один dialOne, allLeaseHeld
				// копится по одной попытке на endpoint.
				break
			}
			if attempt < c.cfg.MaxAttempts {
				// Debug, не Warn: промежуточный retry — ожидаемый шум; при больших
				// fallback-list + сетевом шторме не должен заваливать Warn-лог.
				// Итоговый «dial failed» (после исчерпания попыток) ниже — Warn.
				c.logger.Debug("eventstream: dial failed, retrying same endpoint",
					slog.String("addr", ep.Addr),
					slog.Int("attempt", attempt),
					slog.Int("max_attempts", c.cfg.MaxAttempts),
					slog.Any("error", err),
				)
				if !c.sleepInterAttempt(ctx) {
					// ctx отменён во время паузы — выходим, итог отдаём ниже.
					dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", ep.Addr, ctx.Err()))
					return nil, fmt.Errorf("grpc: client: all endpoints failed:\n  - %s", strings.Join(dialErrs, "\n  - "))
				}
			}
		}
		// spray: AlreadyExists на одном endpoint НЕ прерывает перебор — следующий
		// endpoint мог уже перехватить lease после force-release. Флаг сбрасываем,
		// как только хоть один фейл — НЕ AlreadyExists (тогда это transport-сбой,
		// общий backoff-cap, не модест-cap lease-held ветки).
		if !isAlreadyExists(err) {
			allLeaseHeld = false
		}
		c.logger.Warn("eventstream: dial failed", slog.String("addr", ep.Addr), slog.Any("error", err))
		dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", ep.Addr, err))
	}
	if maxPriority > 0 && len(dialErrs) == 0 {
		// Не нашли endpoint-ов с priority < maxPriority — это нормально, не
		// ошибка failback-цикла; отличаем от случая «есть, но все упали».
		return nil, errNoHigherPriority
	}
	aggregated := fmt.Errorf("grpc: client: all endpoints failed:\n  - %s", strings.Join(dialErrs, "\n  - "))
	if tried && allLeaseHeld {
		// Все пробованные endpoint-ы отдали AlreadyExists — оборачиваем sentinel-ом
		// errLeaseHeld, чтобы reconnect-loop применил модест-cap (быстрый возврат
		// после истечения presence), не теряя диагностику в aggregated. status в
		// per-endpoint обёртках уже стёрт (%v), поэтому сигнал несём через sentinel.
		return nil, fmt.Errorf("%w: %w", errLeaseHeld, aggregated)
	}
	return nil, aggregated
}

// errNoHigherPriority — sentinel-ошибка DialPriority: нет endpoint-ов с
// priority < maxPriority. Используется failback-loop-ом, чтобы отличить
// «некуда возвращаться» от «все попытки провалились».
var errNoHigherPriority = errors.New("grpc: client: no higher-priority endpoint")

// IsNoHigherPriority — публичный selector над sentinel-ошибкой DialPriority.
func IsNoHigherPriority(err error) bool {
	return errors.Is(err, errNoHigherPriority)
}

// errLeaseHeld — sentinel-ошибка Dial/DialPriority: ВСЕ пробованные endpoint-ы
// отвергли handshake с gRPC AlreadyExists (SID-lease ещё держит живой/не-
// истёкший holder, keeper/internal/grpc/eventstream.go::acquireSoulLease). Dial
// удался на транспорте, но сессия отвергнута — soft-failure для целей backoff:
// reconnect-loop применяет модест-cap вместо общего transport-cap, чтобы Soul
// переподключился в пределах секунд после истечения presence (force-release),
// а не долбил выживших keeper-ов и не ждал раздутый exponential-cap.
var errLeaseHeld = errors.New("grpc: client: soul lease held by live keeper")

// IsLeaseHeld — публичный selector над sentinel-ошибкой errLeaseHeld.
func IsLeaseHeld(err error) bool {
	return errors.Is(err, errLeaseHeld)
}

// isAlreadyExists — true, если ошибка несёт gRPC-status codes.AlreadyExists
// где-либо в chain (dialOne оборачивает recv-ошибку через %w, status сохраняется).
func isAlreadyExists(err error) bool {
	return status.Code(err) == codes.AlreadyExists
}

// isRetriablePerEndpoint решает, повторять ли dialOne к ТОМУ ЖЕ endpoint-у
// (per-endpoint retry, keeper.retry.max_attempts) или сразу spray-ить к
// следующему. Матрица нормативна (architect, docs/soul/connection.md):
//
//   - НЕ retriable (повтор к тому же endpoint бессмыслен → break, spray дальше):
//     AlreadyExists (lease-held — другой keeper держит SID-lease),
//     Unauthenticated / PermissionDenied (auth-проблема не самоисправится),
//     InvalidArgument / FailedPrecondition / Unimplemented (контрактный отказ).
//   - retriable (transient transport-flake — повтор может пройти):
//     Unavailable / DeadlineExceeded / Internal / Unknown / Aborted, а также
//     локальный handshake-timeout (не gRPC-status: dialOne отдаёт обычный
//     fmt.Errorf, status.Code → codes.Unknown, попадает в default → retriable).
//   - default (неклассифицированный код) → retriable, консервативно.
func isRetriablePerEndpoint(err error) bool {
	switch status.Code(err) {
	case codes.AlreadyExists,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.InvalidArgument,
		codes.FailedPrecondition,
		codes.Unimplemented:
		return false
	default:
		// Unavailable / DeadlineExceeded / Internal / Unknown / Aborted +
		// handshake-timeout (codes.Unknown) + всё неклассифицированное.
		return true
	}
}

// sleepInterAttempt выдерживает плоскую паузу InterAttemptDelay (±jitter) между
// попытками к одному endpoint-у, прерываясь по ctx. Возвращает true, если пауза
// выдержана, false — ctx отменён. Рост cap-а остаётся внешнему reconnectLoop —
// здесь пауза плоская (reuse backoff.initial).
func (c *Client) sleepInterAttempt(ctx context.Context) bool {
	d := c.cfg.InterAttemptDelay
	if c.cfg.InterAttemptJitter && d > 0 {
		delta := d / 4
		d = d + time.Duration(rand.Int64N(int64(delta*2))) - delta
	}
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

// dialOne — одна попытка к одному endpoint-у. Stream живёт под собственным
// session-ctx-ом (cancel в StreamSession.Close); handshake-timeout
// накладывается локально через select-on-channel, иначе timeout-cancel убил
// бы и долгоживущий стрим.
func (c *Client) dialOne(ctx context.Context, addr string, creds credentials.TransportCredentials, kp keepalive.ClientParameters) (*StreamSession, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(kp),
		// recv-лимит входящего ApplyRequest (config.keeper.max_apply_size_mb).
		// gRPC-дефолт recv (4 MiB) мал для крупного Destiny; send-лимит клиента
		// не трогаем — Soul шлёт только мелкие FromSoul (TaskEvent/RunResult).
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(c.cfg.MaxRecvMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	stream, err := keeperv1.NewKeeperClient(conn).EventStream(sessCtx)
	if err != nil {
		sessCancel()
		_ = conn.Close()
		return nil, fmt.Errorf("EventStream: %w", err)
	}

	hsDone := make(chan error, 1)
	var reply *keeperv1.HelloReply
	go func() {
		hello := &keeperv1.Hello{
			SidEcho:     c.cfg.SID,
			SoulVersion: c.cfg.SoulVersion,
			// Анонс фичей протокола (ADR-056 §S5): keeper персистит набор рядом с
			// presence и сверяет ДО dispatch-а staged-сценария. Без "passage" этот
			// Soul под N>1 Passage отвергается fail-closed, а не зависает.
			Capabilities: config.SoulCapabilities(),
		}
		if err := stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_Hello{Hello: hello}}); err != nil {
			hsDone <- fmt.Errorf("send Hello: %w", err)
			return
		}
		first, err := stream.Recv()
		if err != nil {
			hsDone <- fmt.Errorf("recv HelloReply: %w", err)
			return
		}
		r := first.GetHelloReply()
		if r == nil {
			hsDone <- fmt.Errorf("expected HelloReply, got %T", first.GetPayload())
			return
		}
		reply = r
		hsDone <- nil
	}()
	select {
	case err := <-hsDone:
		if err != nil {
			sessCancel()
			_ = conn.Close()
			return nil, err
		}
	case <-time.After(c.cfg.HandshakeTimeout):
		sessCancel()
		_ = conn.Close()
		return nil, fmt.Errorf("handshake timeout %s", c.cfg.HandshakeTimeout)
	}

	return &StreamSession{
		conn:       conn,
		stream:     stream,
		cancel:     sessCancel,
		kid:        reply.GetKid(),
		sessionID:  reply.GetSessionId(),
		serverTime: reply.GetServerTime().AsTime(),
		logger:     c.logger,
	}, nil
}

// orderedEndpoints возвращает endpoints, отсортированные по приоритету
// (меньше → раньше), с in-priority shuffle (spray, ADR-002). Не мутирует
// исходный slice.
func orderedEndpoints(in []Endpoint) []Endpoint {
	out := make([]Endpoint, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		return normalizedPriority(out[i].Priority) < normalizedPriority(out[j].Priority)
	})
	// Spray: shuffle внутри каждой priority-группы.
	for i := 0; i < len(out); {
		j := i + 1
		pi := normalizedPriority(out[i].Priority)
		for j < len(out) && normalizedPriority(out[j].Priority) == pi {
			j++
		}
		if j-i > 1 {
			group := out[i:j]
			rand.Shuffle(len(group), func(a, b int) { group[a], group[b] = group[b], group[a] })
		}
		i = j
	}
	return out
}

func normalizedPriority(p int) int {
	if p == 0 {
		return 1
	}
	return p
}

// hostFromAddr — host из `host:port` для SNI / hostname-verify.
// Дубликат соседнего helper-а из internal/bootstrap; выносить в shared ради
// 6 строк — overkill.
func hostFromAddr(s string) (string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", false
	}
	host := s[:i]
	if strings.Contains(host, ":") {
		return "", false
	}
	return host, true
}

// StreamSession — открытый bidi-стрим Keeper↔Soul с уже завершённым
// handshake-ом. Реализует runtime.EventSink (SendTaskEvent / SendRunResult).
//
// Concurrent Send-ы сериализуются внутренним writeMu: gRPC bidi-stream не
// допускает concurrent Send без внешней синхронизации. В MVP большинство
// Send-ов идёт из единственного writer-а (select-loop handleSession), но
// Errand-горутина (ADR-033) — параллельный writer: апплай ещё может идти, а
// Errand уже пришёл. writeMu закрывает гонку без перестройки архитектуры
// «один writer».
type StreamSession struct {
	conn       *grpc.ClientConn
	stream     grpc.BidiStreamingClient[keeperv1.FromSoul, keeperv1.FromKeeper]
	cancel     context.CancelFunc
	kid        string
	sessionID  string
	serverTime time.Time
	priority   int
	logger     *slog.Logger

	// writeMu сериализует Send (ApplyRunner.SendTaskEvent / SendRunResult,
	// SendWardRoster, SendSoulprintReport, SendFromSoul, SendErrandResult).
	// Read-loop (Recv) — отдельная горутина, мьютекс не нужен. Recv и Send
	// gRPC bidi-stream-а параллелятся независимо (контракт google.golang.org/
	// grpc).
	writeMu sync.Mutex
}

func (s *StreamSession) KID() string       { return s.kid }
func (s *StreamSession) SessionID() string { return s.sessionID }
func (s *StreamSession) ServerTime() time.Time {
	return s.serverTime
}

// Priority — приоритет endpoint-а, на котором установлена сессия (после
// нормализации zero→1). 0 — сессия открыта без учёта приоритета (legacy-Dial).
func (s *StreamSession) Priority() int { return s.priority }

// Recv возвращает следующее сообщение FromKeeper. io.EOF — нормальное
// закрытие; любая другая ошибка — повод reconnect в caller-loop.
func (s *StreamSession) Recv() (*keeperv1.FromKeeper, error) {
	msg, err := s.stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return msg, err
}

// SendTaskEvent отправляет TaskEvent в Keeper. Удовлетворяет runtime.EventSink.
func (s *StreamSession) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_TaskEvent{TaskEvent: ev}})
}

// SendRunResult отправляет RunResult. Удовлетворяет runtime.EventSink.
func (s *StreamSession) SendRunResult(r *keeperv1.RunResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_RunResult{RunResult: r}})
}

// SendFromSoul отправляет произвольный FromSoul на стрим. Нужен Augur-клиенту
// (soul/internal/augur) для отсылки AugurRequest — он несёт payload, не
// покрытый узкими Send*-хелперами. Сериализуется writeMu (concurrent Send из
// apply-горутины и Errand-горутины безопасен).
func (s *StreamSession) SendFromSoul(msg *keeperv1.FromSoul) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(msg)
}

// SendWardRoster шлёт снимок ведомых apply-прогонов (ReplaceAll, Soul-reconcile
// ADR-027(g), S6). Вызывается caller-ом (handleSession) СРАЗУ после handshake и
// ДО первого app-сообщения на каждом (re)connect-е: Keeper по нему терминалит
// осиротевшие dispatched-строки SID-а. active=nil → WardRoster с пустым набором
// (явная декларация «ничего не ведётся»).
func (s *StreamSession) SendWardRoster(active []*keeperv1.ActiveApply) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_WardRoster{
			WardRoster: &keeperv1.WardRoster{Active: active},
		},
	})
}

// SendSoulprintReport — для будущего soulprint-collector-а (M2.3+).
// Заложен симметрично с TaskEvent, на текущий момент cmd/soul его не зовёт.
// `received_at` — Keeper-only поле (ADR-018), здесь не выставляется.
func (s *StreamSession) SendSoulprintReport(rep *keeperv1.SoulprintReport) error {
	if rep.GetCollectedAt() == nil {
		rep.CollectedAt = timestamppb.Now()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_SoulprintReport{SoulprintReport: rep}})
}

// SendHostUtilization — снимок живой утилизации хоста (ADR-071). Зеркало
// SendSoulprintReport: `received_at` — Keeper-only, здесь не выставляется.
func (s *StreamSession) SendHostUtilization(u *keeperv1.HostUtilization) error {
	if u.GetCollectedAt() == nil {
		u.CollectedAt = timestamppb.Now()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_HostUtilization{HostUtilization: u}})
}

// SendErrandResult шлёт финальный ErrandResult Keeper-у (ADR-033, slice E3).
// Errand-горутина (recv-handler в cmd/soul) вызывает этот метод параллельно
// apply-горутине, поэтому writeMu обязателен — concurrent Send в gRPC bidi-
// stream рассогласовал бы протокол. Один ErrandRequest → один ErrandResult.
func (s *StreamSession) SendErrandResult(r *keeperv1.ErrandResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_ErrandResult{ErrandResult: r}})
}

// FetchModule открывает server-streaming fetch байтов SoulModule-плагина по
// той же mTLS-ClientConn, что EventStream (ADR-065(a)): артефакт едет отдельным
// HTTP/2-стримом и не душит control-plane. writeMu не нужен — это независимый
// RPC, не Send в bidi-stream. Реализует coremod/module.Fetcher.
func (s *StreamSession) FetchModule(ctx context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error) {
	return keeperv1.NewKeeperClient(s.conn).FetchModule(ctx, req)
}

// Close корректно завершает сессию: CloseSend → cancel ctx → conn.Close.
// Идемпотентен.
func (s *StreamSession) Close() error {
	if s.stream != nil {
		_ = s.stream.CloseSend()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
