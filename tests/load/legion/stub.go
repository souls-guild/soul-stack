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

// stubKeepalive -- the same parameters as the real Soul client
// (soul/internal/grpc/client.go): ping every 30s after idle,
// PermitWithoutStream. 30s > server MinTime 10s (eventStreamKeepaliveMinTime)
// -- too_many_pings GOAWAY is excluded. Across N streams this is exactly
// axis-A presence load (gRPC keepalive + app message update last_seen_at,
// ADR-012).
var stubKeepalive = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// Stub -- one fake-Soul: holds a long-lived EventStream to Keeper, sends
// Hello + (optional) SoulprintReport, replies with a scripted
// RunResult{SUCCESS, changed=false} to ApplyRequest. Does NOT parse Destiny,
// does NOT apply (load contract).
type Stub struct {
	id         Identity
	keeperAddr string
	serverName string
	caPool     *x509.CertPool

	mu       sync.Mutex
	conn     *grpc.ClientConn
	stream   keeperv1.Keeper_EventStreamClient
	cancel   context.CancelFunc
	helloAck bool   // HelloReply received
	applies  int    // number of ApplyRequests handled
	errands  int    // number of ErrandRequests handled (command Voyage, axis C)
	recvErr  string // non-empty only on an abnormal recv-loop break (not EOF/Canceled)
}

// NewStub assembles a stub. caBundle -- root CA of the Keeper server cert
// (server-cert verification on handshake); serverName -- SNI/verification
// (on the dev stand the keeper-cert is issued for CN=localhost, SAN
// localhost+127.0.0.1).
func NewStub(id Identity, keeperAddr, serverName string, caBundle []byte) (*Stub, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("legion: failed to add CA to pool")
	}
	return &Stub{
		id:         id,
		keeperAddr: keeperAddr,
		serverName: serverName,
		caPool:     pool,
	}, nil
}

// Open connects over mTLS, opens the EventStream, sends Hello, starts the
// recv-loop. Blocks until HelloReply is confirmed (or ctx error/timeout) --
// Keeper sends HelloReply AFTER acquiring the Redis SID-lease, which is
// exactly "the stream is really accounted for" (keeper_grpc_streams_active.Inc).
// Returns a handshake error.
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

	// Wait for HelloReply: Keeper sends it after acquiring the SID-lease
	// (presence online). Before that the stream is not considered "really
	// connected".
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
			return fmt.Errorf("legion(%s): stream closed before HelloReply", s.id.SID)
		}
		select {
		case <-ctx.Done():
			s.Close()
			return fmt.Errorf("legion(%s): ctx canceled before HelloReply: %w", s.id.SID, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	s.Close()
	return fmt.Errorf("legion(%s): HelloReply not received within 10s", s.id.SID)
}

// SendSoulprint sends a minimal typed SoulprintReport (like a real Soul on
// refresh_interval) -- loads the keeper-side soulprint-upsert into PG.
// Best-effort: send errors are returned but are not fatal for holding the
// stream.
func (s *Stub) SendSoulprint() error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return errors.New("legion: stream closed")
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

// recvLoop reads Keeper's messages. On HelloReply sets ack; on ApplyRequest
// replies with RunResult{SUCCESS} (scenario Voyage); on ErrandRequest --
// ErrandResult{SUCCESS} (command Voyage, axis C load). Other messages are
// ignored (load does NOT implement Vigil/Sigil/Augur -- we measure dispatch
// load on Keeper, not apply realism, docs/testing/load-testing.md §3).
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
			// EOF/Canceled -- normal close (CloseSend/Close teardown). Any
			// other error -- an abnormal stream break by Keeper
			// (RST/GOAWAY/Unavailable): recorded to distinguish "holding N"
			// from "Keeper dropped some streams under load".
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

// respondApply sends an aggregated RunResult{SUCCESS} with no state_changes
// (changed=false). Does NOT execute tasks -- we measure keeper-side
// dispatch->RunResult, not apply realism (load contract,
// docs/testing/load-testing.md §3).
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

// respondErrand replies to an ErrandRequest (command Voyage, ADR-033/043)
// with ErrandResult{SUCCESS, exit_code=0} -- echoes errand_id from the
// request. Does NOT execute shell/exec: we measure keeper-side
// dispatch->ErrandResult->voyage-target-terminal->audit, not module
// execution realism (load contract, reference implementation --
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

// Connected -- whether the stream is accounted for (HelloReply received and
// the stream is still alive).
func (s *Stub) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helloAck && s.stream != nil
}

// Applies -- number of ApplyRequests handled (for the axis C-lite report).
func (s *Stub) Applies() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applies
}

// Errands -- number of ErrandRequests handled (command Voyage, axis C load).
func (s *Stub) Errands() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errands
}

// RecvErr -- text of an abnormal recv-loop break (not EOF/Canceled), or ""
// if the stream closed normally or is still alive.
func (s *Stub) RecvErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvErr
}

// SID -- the stub's identifier.
func (s *Stub) SID() string { return s.id.SID }

// Close -- graceful shutdown of the stream. Safe to call repeatedly.
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
