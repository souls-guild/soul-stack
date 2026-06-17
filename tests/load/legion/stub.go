package legion

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// stubKeepalive — те же параметры, что у настоящего Soul-клиента
// (soul/internal/grpc/client.go): ping каждые 30s после idle, PermitWithoutStream.
// 30s > server MinTime 10s (eventStreamKeepaliveMinTime) — too_many_pings GOAWAY
// исключён. Под N стримов это и есть оси-A presence-нагрузка (gRPC keepalive +
// app-сообщение обновляют last_seen_at, ADR-012).
var stubKeepalive = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// Stub — один fake-Soul: держит долгоживущий EventStream к Keeper-у, шлёт Hello +
// (опц.) SoulprintReport, отвечает scripted RunResult{SUCCESS, changed=false} на
// ApplyRequest. НЕ парсит Destiny, НЕ применяет (load-контракт).
type Stub struct {
	id         Identity
	keeperAddr string
	serverName string
	caPool     *x509.CertPool

	mu       sync.Mutex
	conn     *grpc.ClientConn
	stream   keeperv1.Keeper_EventStreamClient
	cancel   context.CancelFunc
	helloAck bool   // получен HelloReply
	applies  int    // сколько ApplyRequest обработано
	errands  int    // сколько ErrandRequest обработано (command-Voyage, ось C)
	recvErr  string // непустая только при НЕштатном обрыве recv-loop (не EOF/Canceled)
}

// NewStub собирает stub. caBundle — root CA Keeper-server-cert-а (верификация
// server-cert-а на handshake-е); serverName — SNI/верификация (на dev-стенде
// keeper-cert выписан на CN=localhost, SAN localhost+127.0.0.1).
func NewStub(id Identity, keeperAddr, serverName string, caBundle []byte) (*Stub, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("legion: не удалось добавить CA в pool")
	}
	return &Stub{
		id:         id,
		keeperAddr: keeperAddr,
		serverName: serverName,
		caPool:     pool,
	}, nil
}

// Open подключается по mTLS, открывает EventStream, шлёт Hello, запускает
// recv-loop. Блокирует до подтверждения HelloReply (или ошибки/таймаута ctx) —
// HelloReply Keeper шлёт ПОСЛЕ захвата Redis SID-lease, что и есть «стрим реально
// учтён» (keeper_grpc_streams_active.Inc). Возвращает ошибку handshake-а.
func (s *Stub) Open(ctx context.Context) error {
	clientCert, err := tls.X509KeyPair(s.id.CertPEM, s.id.KeyPEM)
	if err != nil {
		return fmt.Errorf("legion(%s): X509KeyPair: %w", s.id.SID, err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      s.caPool,
		ServerName:   s.serverName,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.NewClient(s.keeperAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(stubKeepalive),
	)
	if err != nil {
		return fmt.Errorf("legion(%s): grpc.NewClient: %w", s.id.SID, err)
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	client := keeperv1.NewKeeperClient(conn)
	stream, err := client.EventStream(streamCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("legion(%s): EventStream: %w", s.id.SID, err)
	}

	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{
			Hello: &keeperv1.Hello{
				SidEcho:     s.id.SID,
				SoulVersion: "soul-legion",
			},
		},
	}); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("legion(%s): send Hello: %w", s.id.SID, err)
	}

	s.mu.Lock()
	s.conn = conn
	s.stream = stream
	s.cancel = cancel
	s.mu.Unlock()

	go s.recvLoop()

	// Ждём HelloReply: Keeper присылает его после захвата SID-lease (presence
	// online). До этого момента стрим не считается «реально подключённым».
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ack := s.helloAck
		closed := s.stream == nil
		s.mu.Unlock()
		if ack {
			return nil
		}
		if closed {
			return fmt.Errorf("legion(%s): стрим закрылся до HelloReply", s.id.SID)
		}
		select {
		case <-ctx.Done():
			s.Close()
			return fmt.Errorf("legion(%s): ctx отменён до HelloReply: %w", s.id.SID, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	s.Close()
	return fmt.Errorf("legion(%s): HelloReply не получен за 10s", s.id.SID)
}

// SendSoulprint шлёт минимальный typed-SoulprintReport (как настоящий Soul на
// refresh_interval) — нагружает Keeper-side soulprint-upsert в PG. Best-effort:
// ошибка send-а возвращается, не фатальна для удержания стрима.
func (s *Stub) SendSoulprint() error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return errors.New("legion: стрим закрыт")
	}
	return stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_SoulprintReport{
			SoulprintReport: &keeperv1.SoulprintReport{
				CollectedAt: timestamppb.New(time.Now()),
				TypedFacts: &keeperv1.SoulprintFacts{
					Sid:      s.id.SID,
					Hostname: s.id.SID,
					Os: &keeperv1.OsFacts{
						Family:     "debian",
						Distro:     "ubuntu",
						Version:    "22.04",
						Arch:       "amd64",
						PkgMgr:     "apt",
						InitSystem: "systemd",
					},
				},
			},
		},
	})
}

// recvLoop читает сообщения Keeper-а. На HelloReply ставит ack; на ApplyRequest
// отвечает RunResult{SUCCESS} (scenario-Voyage); на ErrandRequest — ErrandResult
// {SUCCESS} (command-Voyage, ось C run-нагрузки). Прочие сообщения игнорируются
// (load НЕ реализует Vigil/Sigil/Augur — мерим dispatch-нагрузку на Keeper, не
// реализм apply, docs/testing/load-testing.md §3).
func (s *Stub) recvLoop() {
	for {
		s.mu.Lock()
		stream := s.stream
		s.mu.Unlock()
		if stream == nil {
			return
		}
		frame, err := stream.Recv()
		if err != nil {
			// EOF/Canceled — штатное закрытие (CloseSend/Close-teardown). Любая
			// другая ошибка — НЕштатный обрыв стрима Keeper-ом (RST/GOAWAY/
			// Unavailable): фиксируем, чтобы отличить «держим N» от «Keeper
			// сбросил часть стримов под нагрузкой».
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				s.mu.Lock()
				if s.recvErr == "" {
					s.recvErr = err.Error()
				}
				s.mu.Unlock()
			}
			s.markStreamClosed()
			return
		}
		if frame.GetHelloReply() != nil {
			s.mu.Lock()
			s.helloAck = true
			s.mu.Unlock()
			continue
		}
		if req := frame.GetApplyRequest(); req != nil {
			s.respondApply(req)
			continue
		}
		if er := frame.GetErrandRequest(); er != nil {
			s.respondErrand(er)
		}
	}
}

func (s *Stub) markStreamClosed() {
	s.mu.Lock()
	s.stream = nil
	s.mu.Unlock()
}

// respondApply шлёт агрегированный RunResult{SUCCESS} без state_changes
// (changed=false). НЕ исполняет задачи — мерим keeper-side dispatch→RunResult, а
// не реализм apply (load-контракт, docs/testing/load-testing.md §3).
func (s *Stub) respondApply(req *keeperv1.ApplyRequest) {
	s.mu.Lock()
	stream := s.stream
	s.applies++
	s.mu.Unlock()
	if stream == nil {
		return
	}
	_ = stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_RunResult{
			RunResult: &keeperv1.RunResult{
				ApplyId: req.GetApplyId(),
				Status:  keeperv1.RunStatus_RUN_STATUS_SUCCESS,
				Attempt: req.GetAttempt(),
			},
		},
	})
}

// respondErrand отвечает на ErrandRequest (command-Voyage, ADR-033/043)
// ErrandResult{SUCCESS, exit_code=0} — эхо errand_id из запроса. НЕ исполняет
// shell/exec: мерим keeper-side dispatch→ErrandResult→voyage-target-terminal→
// audit, а не реализм исполнения модуля (load-контракт, эталон —
// tests/e2e/internal/soulstub::respondToErrand).
func (s *Stub) respondErrand(req *keeperv1.ErrandRequest) {
	s.mu.Lock()
	stream := s.stream
	s.errands++
	s.mu.Unlock()
	if stream == nil {
		return
	}
	_ = stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ErrandResult{
			ErrandResult: &keeperv1.ErrandResult{
				ErrandId:   req.GetErrandId(),
				Status:     keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS,
				ExitCode:   0,
				Stdout:     "ok\n",
				DurationMs: 1,
			},
		},
	})
}

// Connected — учтён ли стрим (HelloReply получен и стрим ещё жив).
func (s *Stub) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helloAck && s.stream != nil
}

// Applies — сколько ApplyRequest обработано (для report-а оси C-lite).
func (s *Stub) Applies() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applies
}

// Errands — сколько ErrandRequest обработано (command-Voyage, ось C run-нагрузки).
func (s *Stub) Errands() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errands
}

// RecvErr — текст НЕштатного обрыва recv-loop-а (не EOF/Canceled), или "" если
// стрим закрыт штатно либо ещё жив.
func (s *Stub) RecvErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvErr
}

// SID — идентификатор стаба.
func (s *Stub) SID() string { return s.id.SID }

// Close — graceful-shutdown стрима. Безопасен к повторному вызову.
func (s *Stub) Close() {
	s.mu.Lock()
	cancel := s.cancel
	conn := s.conn
	stream := s.stream
	s.stream = nil
	s.conn = nil
	s.cancel = nil
	s.mu.Unlock()

	if stream != nil {
		_ = stream.CloseSend()
	}
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}
