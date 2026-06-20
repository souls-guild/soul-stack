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
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
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
}

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
	for _, ep := range orderedEndpoints(c.cfg.Endpoints) {
		if maxPriority > 0 && normalizedPriority(ep.Priority) >= maxPriority {
			continue
		}
		cfgForAddr := tlsCfg.Clone()
		if h, ok := hostFromAddr(ep.Addr); ok {
			cfgForAddr.ServerName = h
		}
		creds := credentials.NewTLS(cfgForAddr)

		sess, err := c.dialOne(ctx, ep.Addr, creds, kp)
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
		c.logger.Warn("eventstream: dial failed", slog.String("addr", ep.Addr), slog.Any("error", err))
		dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", ep.Addr, err))
	}
	if maxPriority > 0 && len(dialErrs) == 0 {
		// Не нашли endpoint-ов с priority < maxPriority — это нормально, не
		// ошибка failback-цикла; отличаем от случая «есть, но все упали».
		return nil, errNoHigherPriority
	}
	return nil, fmt.Errorf("grpc: client: all endpoints failed:\n  - %s", strings.Join(dialErrs, "\n  - "))
}

// errNoHigherPriority — sentinel-ошибка DialPriority: нет endpoint-ов с
// priority < maxPriority. Используется failback-loop-ом, чтобы отличить
// «некуда возвращаться» от «все попытки провалились».
var errNoHigherPriority = errors.New("grpc: client: no higher-priority endpoint")

// IsNoHigherPriority — публичный selector над sentinel-ошибкой DialPriority.
func IsNoHigherPriority(err error) bool {
	return errors.Is(err, errNoHigherPriority)
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

// SendErrandResult шлёт финальный ErrandResult Keeper-у (ADR-033, slice E3).
// Errand-горутина (recv-handler в cmd/soul) вызывает этот метод параллельно
// apply-горутине, поэтому writeMu обязателен — concurrent Send в gRPC bidi-
// stream рассогласовал бы протокол. Один ErrandRequest → один ErrandResult.
func (s *StreamSession) SendErrandResult(r *keeperv1.ErrandResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stream.Send(&keeperv1.FromSoul{Payload: &keeperv1.FromSoul_ErrandResult{ErrandResult: r}})
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
